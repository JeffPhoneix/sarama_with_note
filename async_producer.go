package sarama

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/eapache/go-resiliency/breaker"
	"github.com/eapache/queue"
)

// AsyncProducer publishes Kafka messages using a non-blocking API. It routes messages
// to the correct broker for the provided topic-partition, refreshing metadata as appropriate,
// and parses responses for errors. You must read from the Errors() channel or the
// producer will deadlock. You must call Close() or AsyncClose() on a producer to avoid
// leaks and message lost: it will not be garbage-collected automatically when it passes
// out of scope and buffered messages may not be flushed.
// 使用一个非阻塞的API来发送kafka消息。
// 将消息为提供的topic-paritition以路由到正确的broker，合适的时候刷新metadata，并为错误解析响应。
// 你必须从Errors channel读取，否则producer就会死锁。
// 你必须在producer实例上调用Close或者AsyncClose() 以避免泄露以及消息丢失。
//   这些东西当传出了作用域之后是不会被自动GC掉的，并且缓冲的消息也不会被刷走。
type AsyncProducer interface {

	// AsyncClose triggers a shutdown of the producer. The shutdown has completed
	// when both the Errors and Successes channels have been closed. When calling
	// AsyncClose, you *must* continue to read from those channels in order to
	// drain the results of any messages in flight.
	// 触发 producer实例的 shutdown。
	// 当Errors和Success channels都已经被关闭的时候， shutdown结束。
	// 当调用 AsyncClose 你必须持续从哪些channels中读取，为了排干任何 in flight中的消息的结果。
	AsyncClose()

	// Close shuts down the producer and waits for any buffered messages to be
	// flushed. You must call this function before a producer object passes out of
	// scope, as it may otherwise leak memory. You must call this before process
	// shutting down, or you may lose messages. You must call this before calling
	// Close on the underlying client.
	// Close shut down producer 并等待任何消息被flushed掉。
	//
	Close() error

	// Input is the input channel for the user to write messages to that they
	// wish to send.
	// Input是输入channel  用于给用户写入消息的
	Input() chan<- *ProducerMessage

	// Successes is the success output channel back to the user when Return.Successes is
	// enabled. If Return.Successes is true, you MUST read from this channel or the
	// Producer will deadlock. It is suggested that you send and read messages
	// together in a single select statement.
	// 当Return.Successes启用的时候， 这个就是成功的输出channel。
	// 如果 Return.Successes 配置为true，你必须从这个channel中读取  否则producer就会死锁
	// 建议你在单条select语句中发送和读取消息。
	Successes() <-chan *ProducerMessage

	// Errors is the error output channel back to the user. You MUST read from this
	// channel or the Producer will deadlock when the channel is full. Alternatively,
	// you can set Producer.Return.Errors in your config to false, which prevents
	// errors to be returned.
	// 就是返回给用户的 error 输出channel。
	// 你必须从这个channel读取，否则Producer当这个channel满了就死锁了。
	// 除非你设置Reture.Errors为false，这样会阻止错误返回。
	Errors() <-chan *ProducerError
}

// transactionManager keeps the state necessary to ensure idempotent production
// 保存为了确保幂等性生产所必须的state
// 事务管理器
type transactionManager struct {
	producerID int64
	// 特别 注意这个 producer epoch
	producerEpoch int16
	// 无非就是用 producer-partititon 作为key来管理每个的这种序列号， 不过这个就是一个int32类型的数而已 回绕或者溢出呢
	sequenceNumbers map[string]int32
	mutex           sync.Mutex
}

const (
	noProducerID    = -1
	noProducerEpoch = -1
)

// 无非就是通过 transaction manager 来专门管理这种 序列号的东西
func (t *transactionManager) getAndIncrementSequenceNumber(topic string, partition int32) (int32, int16) {
	// 管理的维度就是 topic-partition
	key := fmt.Sprintf("%s-%d", topic, partition)
	t.mutex.Lock()
	defer t.mutex.Unlock()
	sequence := t.sequenceNumbers[key]
	t.sequenceNumbers[key] = sequence + 1
	return sequence, t.producerEpoch
}

func (t *transactionManager) bumpEpoch() {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.producerEpoch++
	for k := range t.sequenceNumbers {
		t.sequenceNumbers[k] = 0
	}
}

func (t *transactionManager) getProducerID() (int64, int16) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return t.producerID, t.producerEpoch
}

func newTransactionManager(conf *Config, client Client) (*transactionManager, error) {
	txnmgr := &transactionManager{
		producerID:    noProducerID,
		producerEpoch: noProducerEpoch,
	}

	if conf.Producer.Idempotent {
		initProducerIDResponse, err := client.InitProducerID()
		if err != nil {
			return nil, err
		}
		txnmgr.producerID = initProducerIDResponse.ProducerID
		txnmgr.producerEpoch = initProducerIDResponse.ProducerEpoch
		txnmgr.sequenceNumbers = make(map[string]int32)
		txnmgr.mutex = sync.Mutex{}

		Logger.Printf("Obtained a ProducerId: %d and ProducerEpoch: %d\n", txnmgr.producerID, txnmgr.producerEpoch)
	}

	return txnmgr, nil
}

// AsyncProducer的真正实现类
type asyncProducer struct {
	// 一个实例无非内部肯定有一个client用来处理真正的通信过程
	client Client
	// 就是应用层的一个 配置对象
	conf *Config

	errors                    chan *ProducerError
	input, successes, retries chan *ProducerMessage
	inFlight                  sync.WaitGroup

	brokers    map[*Broker]*brokerProducer
	brokerRefs map[*brokerProducer]int
	brokerLock sync.Mutex

	txnmgr *transactionManager
}

// NewAsyncProducer creates a new AsyncProducer using the given broker addresses and configuration.
func NewAsyncProducer(addrs []string, conf *Config) (AsyncProducer, error) {
	// 无非就是两步  就是先创建出一个 client  client无非就是底层的所有连接的管理而已
	client, err := NewClient(addrs, conf)
	if err != nil {
		return nil, err
	}
	return newAsyncProducer(client)
}

// NewAsyncProducerFromClient creates a new Producer using the given client. It is still
// necessary to call Close() on the underlying client when shutting down this producer.
// 是否 可以 其实创建的 所有 producer 都共用这个 client对象 然后上层的AsyncProducer自己来处理自己的事情呢
func NewAsyncProducerFromClient(client Client) (AsyncProducer, error) {
	// For clients passed in by the client, ensure we don't
	// call Close() on it.
	cli := &nopCloserClient{client}
	return newAsyncProducer(cli)
}

func newAsyncProducer(client Client) (AsyncProducer, error) {
	// Check that we are not dealing with a closed Client before processing any other arguments
	if client.Closed() {
		return nil, ErrClosedClient
	}

	txnmgr, err := newTransactionManager(client.Config(), client)
	if err != nil {
		return nil, err
	}

	// 无非就是一个 这种 async Producer实例而已 同时启动了 两个协程而已
	p := &asyncProducer{
		client:     client,
		conf:       client.Config(),
		errors:     make(chan *ProducerError),
		input:      make(chan *ProducerMessage),
		successes:  make(chan *ProducerMessage),
		retries:    make(chan *ProducerMessage),
		brokers:    make(map[*Broker]*brokerProducer),
		brokerRefs: make(map[*brokerProducer]int),
		txnmgr:     txnmgr,
	}

	// launch our singleton dispatchers
	// 也就是每个 Producer内部都有这种协程专门来处理 输入 并传给底层的client 实例的
	go withRecover(p.dispatcher)
	go withRecover(p.retryHandler)

	return p, nil
}

type flagSet int8

const (
	syn      flagSet = 1 << iota // first message from partitionProducer to brokerProducer
	fin                          // final message from partitionProducer to brokerProducer and back
	shutdown                     // start the shutdown process
)

// ProducerMessage is the collection of elements passed to the Producer in order to send a message.
type ProducerMessage struct {
	Topic string // The Kafka topic for this message.
	// The partitioning key for this message. Pre-existing Encoders include
	// StringEncoder and ByteEncoder.
	Key Encoder
	// The actual message to store in Kafka. Pre-existing Encoders include
	// StringEncoder and ByteEncoder.
	Value Encoder

	// The headers are key-value pairs that are transparently passed
	// by Kafka between producers and consumers.
	Headers []RecordHeader

	// This field is used to hold arbitrary data you wish to include so it
	// will be available when receiving on the Successes and Errors channels.
	// Sarama completely ignores this field and is only to be used for
	// pass-through data.
	Metadata interface{}

	// Below this point are filled in by the producer as the message is processed

	// Offset is the offset of the message stored on the broker. This is only
	// guaranteed to be defined if the message was successfully delivered and
	// RequiredAcks is not NoResponse.
	Offset int64
	// Partition is the partition that the message was sent to. This is only
	// guaranteed to be defined if the message was successfully delivered.
	// 在TopicProducer中为这个消息 分配分区，并且写入到这个字段中``
	// 就是这条消息要发送到的Partition
	// 仅当消息被成功投递的时候，才会被定义，否则一定是没有定义的
	Partition int32
	// Timestamp can vary in behavior depending on broker configuration, being
	// in either one of the CreateTime or LogAppendTime modes (default CreateTime),
	// and requiring version at least 0.10.0.
	//
	// When configured to CreateTime, the timestamp is specified by the producer
	// either by explicitly setting this field, or when the message is added
	// to a produce set.
	//
	// When configured to LogAppendTime, the timestamp assigned to the message
	// by the broker. This is only guaranteed to be defined if the message was
	// successfully delivered and RequiredAcks is not NoResponse.
	Timestamp time.Time

	// 这个应该是消息当前的重试次数记录？
	retries        int
	flags          flagSet
	expectation    chan *ProducerError
	sequenceNumber int32
	producerEpoch  int16
	// 消息是否有序列号？
	hasSequence bool
}

const producerMessageOverhead = 26 // the metadata overhead of CRC, flags, etc.

func (m *ProducerMessage) byteSize(version int) int {
	var size int
	if version >= 2 {
		size = maximumRecordOverhead
		for _, h := range m.Headers {
			size += len(h.Key) + len(h.Value) + 2*binary.MaxVarintLen32
		}
	} else {
		size = producerMessageOverhead
	}
	if m.Key != nil {
		size += m.Key.Length()
	}
	if m.Value != nil {
		size += m.Value.Length()
	}
	return size
}

func (m *ProducerMessage) clear() {
	m.flags = 0
	m.retries = 0
	m.sequenceNumber = 0
	m.producerEpoch = 0
	m.hasSequence = false
}

// ProducerError is the type of error generated when the producer fails to deliver a message.
// It contains the original ProducerMessage as well as the actual error value.
type ProducerError struct {
	Msg *ProducerMessage
	Err error
}

func (pe ProducerError) Error() string {
	return fmt.Sprintf("kafka: Failed to produce message to topic %s: %s", pe.Msg.Topic, pe.Err)
}

func (pe ProducerError) Unwrap() error {
	return pe.Err
}

// ProducerErrors is a type that wraps a batch of "ProducerError"s and implements the Error interface.
// It can be returned from the Producer's Close method to avoid the need to manually drain the Errors channel
// when closing a producer.
type ProducerErrors []*ProducerError

func (pe ProducerErrors) Error() string {
	return fmt.Sprintf("kafka: Failed to deliver %d messages.", len(pe))
}

func (p *asyncProducer) Errors() <-chan *ProducerError {
	return p.errors
}

func (p *asyncProducer) Successes() <-chan *ProducerMessage {
	return p.successes
}

func (p *asyncProducer) Input() chan<- *ProducerMessage {
	// 这个就是 返回用来写入的chan的
	return p.input
}

func (p *asyncProducer) Close() error {
	p.AsyncClose()

	if p.conf.Producer.Return.Successes {
		go withRecover(func() {
			for range p.successes {
			}
		})
	}

	var errors ProducerErrors
	if p.conf.Producer.Return.Errors {
		for event := range p.errors {
			errors = append(errors, event)
		}
	} else {
		<-p.errors
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

func (p *asyncProducer) AsyncClose() {
	go withRecover(p.shutdown)
}

// singleton
// dispatches messages by topic
func (p *asyncProducer) dispatcher() {
	handlers := make(map[string]chan<- *ProducerMessage)
	shuttingDown := false

	// 无非就是专门有一个协程 来从这里读取 要发送的消息
	for msg := range p.input {
		if msg == nil {
			Logger.Println("Something tried to send a nil message, it was ignored.")
			continue
		}

		// 重点就是看这个传入进来的每条消息是如何处理的呢

		if msg.flags&shutdown != 0 {
			shuttingDown = true
			p.inFlight.Done()
			continue
		} else if msg.retries == 0 {
			if shuttingDown {
				// we can't just call returnError here because that decrements the wait group,
				// which hasn't been incremented yet for this message, and shouldn't be
				pErr := &ProducerError{Msg: msg, Err: ErrShuttingDown}
				if p.conf.Producer.Return.Errors {
					p.errors <- pErr
				} else {
					Logger.Println(pErr)
				}
				continue
			}
			p.inFlight.Add(1)
		}

		for _, interceptor := range p.conf.Producer.Interceptors {
			msg.safelyApplyInterceptor(interceptor)
		}

		version := 1
		if p.conf.Version.IsAtLeast(V0_11_0_0) {
			version = 2
		} else if msg.Headers != nil {
			p.returnError(msg, ConfigurationError("Producing headers requires Kafka at least v0.11"))
			continue
		}
		// 无非就是推断出版本，然后用这个来判断是否 消息符合这个最大消息的限制
		if msg.byteSize(version) > p.conf.Producer.MaxMessageBytes {
			p.returnError(msg, ErrMessageSizeTooLarge)
			continue
		}

		// 无非就是按照topic维度将消息通过chan发送给到  topicProducer去

		handler := handlers[msg.Topic]
		if handler == nil {
			// 无非就是内部有给每个 topic 都创建出了一个这种chan实例
			// 所以消息里面的 topic就是干这个用的 无非 就是每个topic都是隔离开的了
			// 也就只有这 topic的第一条消息会创建出 这个topic而已
			handler = p.newTopicProducer(msg.Topic)
			handlers[msg.Topic] = handler
		}

		// 最后这条消息就是 发送到这个 chan 中去了呢
		handler <- msg
	}

	for _, handler := range handlers {
		close(handler)
	}
}

// one per topic
// partitions messages, then dispatches them by partition
type topicProducer struct {
	// 无非就是谁创建了 topicProducer 当然是 asyncProducer
	// 所以用parent 指向创建这个 topicProducer的asyncProducer
	parent *asyncProducer
	topic  string
	input  <-chan *ProducerMessage

	// 好像很多东西都会有这样一个断路器的东西
	breaker *breaker.Breaker
	//  都不直接管理 下面的 Producer实例 而是就维护一个chan的map就行
	handlers map[int32]chan<- *ProducerMessage
	// 分区器 实例 无非就是用来做分区而已
	partitioner Partitioner
}

// 无非就是有几层的这种  这里为每个 topic都有创建一个  chan ProducerMessage
// 也就是每个topic都有一个 topic producer 实例
func (p *asyncProducer) newTopicProducer(topic string) chan<- *ProducerMessage {
	// 所以这个所谓 topic producer 实际上就是一条chan而已
	// 实际上 这个配置就是  topic producer的 chan大小
	input := make(chan *ProducerMessage, p.conf.ChannelBufferSize)
	// 无非就是创建这个对象， 然后返回一条chan而已
	tp := &topicProducer{
		parent: p,
		topic:  topic,
		// 消息的传入 从 asyncProducer 传入到这个 topicProducer的这种chan中
		input:       input,
		breaker:     breaker.New(3, 1, 10*time.Second),
		handlers:    make(map[int32]chan<- *ProducerMessage),
		partitioner: p.conf.Producer.Partitioner(topic),
	}
	// 无非就是启动的时候 就会创建出一个携程专门来处理从 asyncProducer -> topicProducer ->
	go withRecover(tp.dispatch)
	// 这条chan实际上就是会让
	return input
}

// 无非就是 分层还有一个  topic producer
func (tp *topicProducer) dispatch() {
	for msg := range tp.input {
		// 如果是第一次发送消息
		if msg.retries == 0 {
			// 将这条消息 分区到 Partition，并且更新此消息的 Partition字段
			// topic 维度无非就 给它决定了partition 然后将这个东西给送到底层的 partitionProducer实例去而已
			if err := tp.partitionMessage(msg); err != nil {
				tp.parent.returnError(msg, err)
				continue
			}
		}

		handler := tp.handlers[msg.Partition]
		if handler == nil {
			// 特别注意这里还是用 parent  也就是 asyncProducer 进行管理，为什么呢
			// 无非就还是想让他们都知道自己的父亲是谁呢？？
			handler = tp.parent.newPartitionProducer(msg.Topic, msg.Partition)
			tp.handlers[msg.Partition] = handler
		}

		handler <- msg
	}

	for _, handler := range tp.handlers {
		close(handler)
	}
}

func (tp *topicProducer) partitionMessage(msg *ProducerMessage) error {
	var partitions []int32

	// 后续重点研究这里做了什么不得了的事情 拿到了分区列表
	err := tp.breaker.Run(func() (err error) {
		requiresConsistency := false
		if ep, ok := tp.partitioner.(DynamicConsistencyPartitioner); ok {
			requiresConsistency = ep.MessageRequiresConsistency(msg)
		} else {
			requiresConsistency = tp.partitioner.RequiresConsistency()
		}

		if requiresConsistency {
			partitions, err = tp.parent.client.Partitions(msg.Topic)
		} else {
			partitions, err = tp.parent.client.WritablePartitions(msg.Topic)
		}
		return
	})
	if err != nil {
		return err
	}

	numPartitions := int32(len(partitions))
	// 没有分区表  那就说明leader不可用
	if numPartitions == 0 {
		return ErrLeaderNotAvailable
	}

	// 无非就是为这个消息进行分区而已
	choice, err := tp.partitioner.Partition(msg, numPartitions)

	if err != nil {
		return err
	} else if choice < 0 || choice >= numPartitions {
		// 分区出来的是一个无效的分区ID
		return ErrInvalidPartition
	}

	// 最后将这个分区写入到  Partition成员
	msg.Partition = partitions[choice]

	return nil
}

// one per partition per topic
// 1个 <topic, partition> 1个实例
// dispatches messages to the appropriate broker
// 无非就是将消息分发到 合适的 broker去
// also responsible for maintaining message order during retries
// 也会负责维护在重试的时候的消息顺序
type partitionProducer struct {
	// 每个层级的 parent 都是直接到这个 asyncProducer
	// 必须的 很多东西都还是需要通过client去通过底层访问到对应的broker那边去
	parent *asyncProducer
	// 属于哪个 <topic, partition>
	topic     string
	partition int32
	input     <-chan *ProducerMessage

	leader         *Broker
	breaker        *breaker.Breaker
	brokerProducer *brokerProducer

	// highWatermark tracks the "current" retry level, which is the only one where we actually let messages through,
	// all other messages get buffered in retryState[msg.retries].buf to preserve ordering
	// retryState[msg.retries].expectChaser simply tracks whether we've seen a fin message for a given level (and
	// therefore whether our buffer is complete and safe to flush)
	highWatermark int
	retryState    []partitionRetryState
}

type partitionRetryState struct {
	buf          []*ProducerMessage
	expectChaser bool
}

// 还有一个分区的 partitionProducer
func (p *asyncProducer) newPartitionProducer(topic string, partition int32) chan<- *ProducerMessage {
	// 也是这种模式并不直接管理 这个 partitionProducer 而是只知道有这么个chan而已
	input := make(chan *ProducerMessage, p.conf.ChannelBufferSize)
	pp := &partitionProducer{
		parent:    p,
		topic:     topic,
		partition: partition,
		input:     input,

		breaker:    breaker.New(3, 1, 10*time.Second),
		retryState: make([]partitionRetryState, p.conf.Producer.Retry.Max+1),
	}
	go withRecover(pp.dispatch)
	return input
}

func (pp *partitionProducer) backoff(retries int) {
	var backoff time.Duration
	if pp.parent.conf.Producer.Retry.BackoffFunc != nil {
		maxRetries := pp.parent.conf.Producer.Retry.Max
		backoff = pp.parent.conf.Producer.Retry.BackoffFunc(retries, maxRetries)
	} else {
		backoff = pp.parent.conf.Producer.Retry.Backoff
	}
	if backoff > 0 {
		time.Sleep(backoff)
	}
}

func (pp *partitionProducer) dispatch() {
	// try to prefetch the leader; if this doesn't work, we'll do a proper call to `updateLeader`
	// on the first message
	pp.leader, _ = pp.parent.client.Leader(pp.topic, pp.partition)
	if pp.leader != nil {
		pp.brokerProducer = pp.parent.getBrokerProducer(pp.leader)
		pp.parent.inFlight.Add(1) // we're generating a syn message; track it so we don't shut down while it's still inflight
		pp.brokerProducer.input <- &ProducerMessage{Topic: pp.topic, Partition: pp.partition, flags: syn}
	}

	defer func() {
		if pp.brokerProducer != nil {
			pp.parent.unrefBrokerProducer(pp.leader, pp.brokerProducer)
		}
	}()

	for msg := range pp.input {
		if pp.brokerProducer != nil && pp.brokerProducer.abandoned != nil {
			select {
			case <-pp.brokerProducer.abandoned:
				// a message on the abandoned channel means that our current broker selection is out of date
				Logger.Printf("producer/leader/%s/%d abandoning broker %d\n", pp.topic, pp.partition, pp.leader.ID())
				pp.parent.unrefBrokerProducer(pp.leader, pp.brokerProducer)
				pp.brokerProducer = nil
				time.Sleep(pp.parent.conf.Producer.Retry.Backoff)
			default:
				// producer connection is still open.
			}
		}

		if msg.retries > pp.highWatermark {
			// a new, higher, retry level; handle it and then back off
			pp.newHighWatermark(msg.retries)
			pp.backoff(msg.retries)
		} else if pp.highWatermark > 0 {
			// we are retrying something (else highWatermark would be 0) but this message is not a *new* retry level
			if msg.retries < pp.highWatermark {
				// in fact this message is not even the current retry level, so buffer it for now (unless it's a just a fin)
				if msg.flags&fin == fin {
					pp.retryState[msg.retries].expectChaser = false
					pp.parent.inFlight.Done() // this fin is now handled and will be garbage collected
				} else {
					pp.retryState[msg.retries].buf = append(pp.retryState[msg.retries].buf, msg)
				}
				continue
			} else if msg.flags&fin == fin {
				// this message is of the current retry level (msg.retries == highWatermark) and the fin flag is set,
				// meaning this retry level is done and we can go down (at least) one level and flush that
				pp.retryState[pp.highWatermark].expectChaser = false
				pp.flushRetryBuffers()
				pp.parent.inFlight.Done() // this fin is now handled and will be garbage collected
				continue
			}
		}

		// if we made it this far then the current msg contains real data, and can be sent to the next goroutine
		// without breaking any of our ordering guarantees

		if pp.brokerProducer == nil {
			if err := pp.updateLeader(); err != nil {
				pp.parent.returnError(msg, err)
				pp.backoff(msg.retries)
				continue
			}
			Logger.Printf("producer/leader/%s/%d selected broker %d\n", pp.topic, pp.partition, pp.leader.ID())
		}

		// Now that we know we have a broker to actually try and send this message to, generate the sequence
		// number for it.
		// 现在我们知道  我们有一个broker 可以实际试试并实际发这个消息过去。 可以为他生成这个序列号了。
		// All messages being retried (sent or not) have already had their retry count updated
		// 所有被重试的消息不论是否发送或者不发送  都已经有他们自己的重试计数器更新
		// Also, ignore "special" syn/fin messages used to sync the brokerProducer and the topicProducer.
		// 同时忽略掉 特殊的 syn/fin 消息，这种特殊消息用于同步 brokerProducer 和 topicProducer
		if pp.parent.conf.Producer.Idempotent && msg.retries == 0 && msg.flags == 0 {
			// 幂等且 非重试 且非特殊位(syn/fin消息使用的标志位)
			// 无非就是 <topic, partition> 维度下会有一个自增ID 来作为序列号的``
			msg.sequenceNumber, msg.producerEpoch = pp.parent.txnmgr.getAndIncrementSequenceNumber(msg.Topic, msg.Partition)
			msg.hasSequence = true
		}

		// 无非就是partition producer 最总将这个消息发送给  broker producer了
		pp.brokerProducer.input <- msg
	}
}

func (pp *partitionProducer) newHighWatermark(hwm int) {
	Logger.Printf("producer/leader/%s/%d state change to [retrying-%d]\n", pp.topic, pp.partition, hwm)
	pp.highWatermark = hwm

	// send off a fin so that we know when everything "in between" has made it
	// back to us and we can safely flush the backlog (otherwise we risk re-ordering messages)
	pp.retryState[pp.highWatermark].expectChaser = true
	pp.parent.inFlight.Add(1) // we're generating a fin message; track it so we don't shut down while it's still inflight
	pp.brokerProducer.input <- &ProducerMessage{Topic: pp.topic, Partition: pp.partition, flags: fin, retries: pp.highWatermark - 1}

	// a new HWM means that our current broker selection is out of date
	Logger.Printf("producer/leader/%s/%d abandoning broker %d\n", pp.topic, pp.partition, pp.leader.ID())
	pp.parent.unrefBrokerProducer(pp.leader, pp.brokerProducer)
	pp.brokerProducer = nil
}

func (pp *partitionProducer) flushRetryBuffers() {
	Logger.Printf("producer/leader/%s/%d state change to [flushing-%d]\n", pp.topic, pp.partition, pp.highWatermark)
	for {
		pp.highWatermark--

		if pp.brokerProducer == nil {
			if err := pp.updateLeader(); err != nil {
				pp.parent.returnErrors(pp.retryState[pp.highWatermark].buf, err)
				goto flushDone
			}
			Logger.Printf("producer/leader/%s/%d selected broker %d\n", pp.topic, pp.partition, pp.leader.ID())
		}

		for _, msg := range pp.retryState[pp.highWatermark].buf {
			pp.brokerProducer.input <- msg
		}

	flushDone:
		pp.retryState[pp.highWatermark].buf = nil
		if pp.retryState[pp.highWatermark].expectChaser {
			Logger.Printf("producer/leader/%s/%d state change to [retrying-%d]\n", pp.topic, pp.partition, pp.highWatermark)
			break
		} else if pp.highWatermark == 0 {
			Logger.Printf("producer/leader/%s/%d state change to [normal]\n", pp.topic, pp.partition)
			break
		}
	}
}

func (pp *partitionProducer) updateLeader() error {
	return pp.breaker.Run(func() (err error) {
		if err = pp.parent.client.RefreshMetadata(pp.topic); err != nil {
			return err
		}

		if pp.leader, err = pp.parent.client.Leader(pp.topic, pp.partition); err != nil {
			return err
		}

		pp.brokerProducer = pp.parent.getBrokerProducer(pp.leader)
		pp.parent.inFlight.Add(1) // we're generating a syn message; track it so we don't shut down while it's still inflight
		pp.brokerProducer.input <- &ProducerMessage{Topic: pp.topic, Partition: pp.partition, flags: syn}

		return nil
	})
}

// one per broker; also constructs an associated flusher
func (p *asyncProducer) newBrokerProducer(broker *Broker) *brokerProducer {
	var (
		input     = make(chan *ProducerMessage)
		bridge    = make(chan *produceSet)
		pending   = make(chan *brokerProducerResponse)
		responses = make(chan *brokerProducerResponse)
	)

	bp := &brokerProducer{
		parent:         p,
		broker:         broker,
		input:          input,
		output:         bridge,
		responses:      responses,
		buffer:         newProduceSet(p),
		currentRetries: make(map[string]map[int32]error),
	}
	go withRecover(bp.run)

	// minimal bridge to make the network response `select`able
	go withRecover(func() {
		// Use a wait group to know if we still have in flight requests
		var wg sync.WaitGroup

		for set := range bridge {
			request := set.buildRequest()

			// Count the in flight requests to know when we can close the pending channel safely
			wg.Add(1)
			// Capture the current set to forward in the callback
			sendResponse := func(set *produceSet) ProduceCallback {
				return func(response *ProduceResponse, err error) {
					// Forward the response to make sure we do not block the responseReceiver
					pending <- &brokerProducerResponse{
						set: set,
						err: err,
						res: response,
					}
					wg.Done()
				}
			}(set)

			// Use AsyncProduce vs Produce to not block waiting for the response
			// so that we can pipeline multiple produce requests and achieve higher throughput, see:
			// https://kafka.apache.org/protocol#protocol_network
			err := broker.AsyncProduce(request, sendResponse)
			if err != nil {
				// Request failed to be sent
				sendResponse(nil, err)
				continue
			}
			// Callback is not called when using NoResponse
			if p.conf.Producer.RequiredAcks == NoResponse {
				// Provide the expected nil response
				sendResponse(nil, nil)
			}
		}
		// Wait for all in flight requests to close the pending channel safely
		wg.Wait()
		close(pending)
	})

	// In order to avoid a deadlock when closing the broker on network or malformed response error
	// we use an intermediate channel to buffer and send pending responses in order
	// This is because the AsyncProduce callback inside the bridge is invoked from the broker
	// responseReceiver goroutine and closing the broker requires such goroutine to be finished
	go withRecover(func() {
		buf := queue.New()
		for {
			if buf.Length() == 0 {
				res, ok := <-pending
				if !ok {
					// We are done forwarding the last pending response
					close(responses)
					return
				}
				buf.Add(res)
			}
			// Send the head pending response or buffer another one
			// so that we never block the callback
			headRes := buf.Peek().(*brokerProducerResponse)
			select {
			case res, ok := <-pending:
				if !ok {
					continue
				}
				buf.Add(res)
				continue
			case responses <- headRes:
				buf.Remove()
				continue
			}
		}
	})

	if p.conf.Producer.Retry.Max <= 0 {
		bp.abandoned = make(chan struct{})
	}

	return bp
}

type brokerProducerResponse struct {
	set *produceSet
	err error
	res *ProduceResponse
}

// groups messages together into appropriately-sized batches for sending to the broker
// handles state related to retries etc
type brokerProducer struct {
	parent *asyncProducer
	broker *Broker

	input     chan *ProducerMessage
	output    chan<- *produceSet
	responses <-chan *brokerProducerResponse
	abandoned chan struct{}

	buffer     *produceSet
	timer      <-chan time.Time
	timerFired bool

	closing        error
	currentRetries map[string]map[int32]error
}

func (bp *brokerProducer) run() {
	var output chan<- *produceSet
	Logger.Printf("producer/broker/%d starting up\n", bp.broker.ID())

	for {
		select {
		case msg, ok := <-bp.input:
			if !ok {
				Logger.Printf("producer/broker/%d input chan closed\n", bp.broker.ID())
				bp.shutdown()
				return
			}

			if msg == nil {
				continue
			}

			if msg.flags&syn == syn {
				Logger.Printf("producer/broker/%d state change to [open] on %s/%d\n",
					bp.broker.ID(), msg.Topic, msg.Partition)
				if bp.currentRetries[msg.Topic] == nil {
					bp.currentRetries[msg.Topic] = make(map[int32]error)
				}
				bp.currentRetries[msg.Topic][msg.Partition] = nil
				bp.parent.inFlight.Done()
				continue
			}

			if reason := bp.needsRetry(msg); reason != nil {
				bp.parent.retryMessage(msg, reason)

				if bp.closing == nil && msg.flags&fin == fin {
					// we were retrying this partition but we can start processing again
					delete(bp.currentRetries[msg.Topic], msg.Partition)
					Logger.Printf("producer/broker/%d state change to [closed] on %s/%d\n",
						bp.broker.ID(), msg.Topic, msg.Partition)
				}

				continue
			}

			if msg.flags&fin == fin {
				// New broker producer that was caught up by the retry loop
				bp.parent.retryMessage(msg, ErrShuttingDown)
				DebugLogger.Printf("producer/broker/%d state change to [dying-%d] on %s/%d\n",
					bp.broker.ID(), msg.retries, msg.Topic, msg.Partition)
				continue
			}

			if bp.buffer.wouldOverflow(msg) {
				Logger.Printf("producer/broker/%d maximum request accumulated, waiting for space\n", bp.broker.ID())
				if err := bp.waitForSpace(msg, false); err != nil {
					bp.parent.retryMessage(msg, err)
					continue
				}
			}

			if bp.parent.txnmgr.producerID != noProducerID && bp.buffer.producerEpoch != msg.producerEpoch {
				// The epoch was reset, need to roll the buffer over
				Logger.Printf("producer/broker/%d detected epoch rollover, waiting for new buffer\n", bp.broker.ID())
				if err := bp.waitForSpace(msg, true); err != nil {
					bp.parent.retryMessage(msg, err)
					continue
				}
			}
			if err := bp.buffer.add(msg); err != nil {
				bp.parent.returnError(msg, err)
				continue
			}

			if bp.parent.conf.Producer.Flush.Frequency > 0 && bp.timer == nil {
				bp.timer = time.After(bp.parent.conf.Producer.Flush.Frequency)
			}
		case <-bp.timer:
			bp.timerFired = true
		case output <- bp.buffer:
			bp.rollOver()
		case response, ok := <-bp.responses:
			if ok {
				bp.handleResponse(response)
			}
		}

		if bp.timerFired || bp.buffer.readyToFlush() {
			output = bp.output
		} else {
			output = nil
		}
	}
}

func (bp *brokerProducer) shutdown() {
	for !bp.buffer.empty() {
		select {
		case response := <-bp.responses:
			bp.handleResponse(response)
		case bp.output <- bp.buffer:
			bp.rollOver()
		}
	}
	close(bp.output)
	// Drain responses from the bridge goroutine
	for response := range bp.responses {
		bp.handleResponse(response)
	}
	// No more brokerProducer related goroutine should be running
	Logger.Printf("producer/broker/%d shut down\n", bp.broker.ID())
}

func (bp *brokerProducer) needsRetry(msg *ProducerMessage) error {
	if bp.closing != nil {
		return bp.closing
	}

	return bp.currentRetries[msg.Topic][msg.Partition]
}

func (bp *brokerProducer) waitForSpace(msg *ProducerMessage, forceRollover bool) error {
	for {
		select {
		case response := <-bp.responses:
			bp.handleResponse(response)
			// handling a response can change our state, so re-check some things
			if reason := bp.needsRetry(msg); reason != nil {
				return reason
			} else if !bp.buffer.wouldOverflow(msg) && !forceRollover {
				return nil
			}
		case bp.output <- bp.buffer:
			bp.rollOver()
			return nil
		}
	}
}

func (bp *brokerProducer) rollOver() {
	bp.timer = nil
	bp.timerFired = false
	bp.buffer = newProduceSet(bp.parent)
}

func (bp *brokerProducer) handleResponse(response *brokerProducerResponse) {
	if response.err != nil {
		bp.handleError(response.set, response.err)
	} else {
		bp.handleSuccess(response.set, response.res)
	}

	if bp.buffer.empty() {
		bp.rollOver() // this can happen if the response invalidated our buffer
	}
}

func (bp *brokerProducer) handleSuccess(sent *produceSet, response *ProduceResponse) {
	// we iterate through the blocks in the request set, not the response, so that we notice
	// if the response is missing a block completely
	var retryTopics []string
	sent.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
		if response == nil {
			// this only happens when RequiredAcks is NoResponse, so we have to assume success
			bp.parent.returnSuccesses(pSet.msgs)
			return
		}

		block := response.GetBlock(topic, partition)
		if block == nil {
			bp.parent.returnErrors(pSet.msgs, ErrIncompleteResponse)
			return
		}

		switch block.Err {
		// Success
		case ErrNoError:
			if bp.parent.conf.Version.IsAtLeast(V0_10_0_0) && !block.Timestamp.IsZero() {
				for _, msg := range pSet.msgs {
					msg.Timestamp = block.Timestamp
				}
			}
			for i, msg := range pSet.msgs {
				msg.Offset = block.Offset + int64(i)
			}
			bp.parent.returnSuccesses(pSet.msgs)
		// Duplicate
		case ErrDuplicateSequenceNumber:
			bp.parent.returnSuccesses(pSet.msgs)
		// Retriable errors
		case ErrInvalidMessage, ErrUnknownTopicOrPartition, ErrLeaderNotAvailable, ErrNotLeaderForPartition,
			ErrRequestTimedOut, ErrNotEnoughReplicas, ErrNotEnoughReplicasAfterAppend:
			if bp.parent.conf.Producer.Retry.Max <= 0 {
				bp.parent.abandonBrokerConnection(bp.broker)
				bp.parent.returnErrors(pSet.msgs, block.Err)
			} else {
				retryTopics = append(retryTopics, topic)
			}
		// Other non-retriable errors
		default:
			if bp.parent.conf.Producer.Retry.Max <= 0 {
				bp.parent.abandonBrokerConnection(bp.broker)
			}
			bp.parent.returnErrors(pSet.msgs, block.Err)
		}
	})

	if len(retryTopics) > 0 {
		if bp.parent.conf.Producer.Idempotent {
			err := bp.parent.client.RefreshMetadata(retryTopics...)
			if err != nil {
				Logger.Printf("Failed refreshing metadata because of %v\n", err)
			}
		}

		sent.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
			block := response.GetBlock(topic, partition)
			if block == nil {
				// handled in the previous "eachPartition" loop
				return
			}

			switch block.Err {
			case ErrInvalidMessage, ErrUnknownTopicOrPartition, ErrLeaderNotAvailable, ErrNotLeaderForPartition,
				ErrRequestTimedOut, ErrNotEnoughReplicas, ErrNotEnoughReplicasAfterAppend:
				Logger.Printf("producer/broker/%d state change to [retrying] on %s/%d because %v\n",
					bp.broker.ID(), topic, partition, block.Err)
				if bp.currentRetries[topic] == nil {
					bp.currentRetries[topic] = make(map[int32]error)
				}
				bp.currentRetries[topic][partition] = block.Err
				if bp.parent.conf.Producer.Idempotent {
					go bp.parent.retryBatch(topic, partition, pSet, block.Err)
				} else {
					bp.parent.retryMessages(pSet.msgs, block.Err)
				}
				// dropping the following messages has the side effect of incrementing their retry count
				bp.parent.retryMessages(bp.buffer.dropPartition(topic, partition), block.Err)
			}
		})
	}
}

func (p *asyncProducer) retryBatch(topic string, partition int32, pSet *partitionSet, kerr KError) {
	Logger.Printf("Retrying batch for %v-%d because of %s\n", topic, partition, kerr)
	produceSet := newProduceSet(p)
	produceSet.msgs[topic] = make(map[int32]*partitionSet)
	produceSet.msgs[topic][partition] = pSet
	produceSet.bufferBytes += pSet.bufferBytes
	produceSet.bufferCount += len(pSet.msgs)
	for _, msg := range pSet.msgs {
		if msg.retries >= p.conf.Producer.Retry.Max {
			p.returnError(msg, kerr)
			return
		}
		msg.retries++
	}

	// it's expected that a metadata refresh has been requested prior to calling retryBatch
	leader, err := p.client.Leader(topic, partition)
	if err != nil {
		Logger.Printf("Failed retrying batch for %v-%d because of %v while looking up for new leader\n", topic, partition, err)
		for _, msg := range pSet.msgs {
			p.returnError(msg, kerr)
		}
		return
	}
	bp := p.getBrokerProducer(leader)
	bp.output <- produceSet
	p.unrefBrokerProducer(leader, bp)
}

func (bp *brokerProducer) handleError(sent *produceSet, err error) {
	var target PacketEncodingError
	if errors.As(err, &target) {
		sent.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
			bp.parent.returnErrors(pSet.msgs, err)
		})
	} else {
		Logger.Printf("producer/broker/%d state change to [closing] because %s\n", bp.broker.ID(), err)
		bp.parent.abandonBrokerConnection(bp.broker)
		_ = bp.broker.Close()
		bp.closing = err
		sent.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
			bp.parent.retryMessages(pSet.msgs, err)
		})
		bp.buffer.eachPartition(func(topic string, partition int32, pSet *partitionSet) {
			bp.parent.retryMessages(pSet.msgs, err)
		})
		bp.rollOver()
	}
}

// singleton
// effectively a "bridge" between the flushers and the dispatcher in order to avoid deadlock
// based on https://godoc.org/github.com/eapache/channels#InfiniteChannel
func (p *asyncProducer) retryHandler() {
	var msg *ProducerMessage
	buf := queue.New()

	for {
		if buf.Length() == 0 {
			msg = <-p.retries
		} else {
			select {
			case msg = <-p.retries:
			case p.input <- buf.Peek().(*ProducerMessage):
				buf.Remove()
				continue
			}
		}

		if msg == nil {
			return
		}

		buf.Add(msg)
	}
}

// utility functions

func (p *asyncProducer) shutdown() {
	Logger.Println("Producer shutting down.")
	p.inFlight.Add(1)
	p.input <- &ProducerMessage{flags: shutdown}

	p.inFlight.Wait()

	err := p.client.Close()
	if err != nil {
		Logger.Println("producer/shutdown failed to close the embedded client:", err)
	}

	close(p.input)
	close(p.retries)
	close(p.errors)
	close(p.successes)
}

func (p *asyncProducer) bumpIdempotentProducerEpoch() {
	_, epoch := p.txnmgr.getProducerID()
	if epoch == math.MaxInt16 {
		Logger.Println("producer/txnmanager epoch exhausted, requesting new producer ID")
		txnmgr, err := newTransactionManager(p.conf, p.client)
		if err != nil {
			Logger.Println(err)
			return
		}

		p.txnmgr = txnmgr
	} else {
		p.txnmgr.bumpEpoch()
	}
}

// 这个函数实际上行 就是 将原封不动的消息 给通过chan 封装成 ProducerError类型通过errors chan给发回去呢
func (p *asyncProducer) returnError(msg *ProducerMessage, err error) {
	// We need to reset the producer ID epoch if we set a sequence number on it, because the broker
	// will never see a message with this number, so we can never continue the sequence.
	// 如果消息有 序列号
	if msg.hasSequence {
		Logger.Printf("producer/txnmanager rolling over epoch due to publish failure on %s/%d", msg.Topic, msg.Partition)
		p.bumpIdempotentProducerEpoch()
	}

	msg.clear()
	pErr := &ProducerError{Msg: msg, Err: err}
	if p.conf.Producer.Return.Errors {
		// 就看是否配置了 返回错误呢
		p.errors <- pErr
	} else {
		Logger.Println(pErr)
	}
	p.inFlight.Done()
}

func (p *asyncProducer) returnErrors(batch []*ProducerMessage, err error) {
	for _, msg := range batch {
		p.returnError(msg, err)
	}
}

func (p *asyncProducer) returnSuccesses(batch []*ProducerMessage) {
	for _, msg := range batch {
		if p.conf.Producer.Return.Successes {
			msg.clear()
			p.successes <- msg
		}
		p.inFlight.Done()
	}
}

func (p *asyncProducer) retryMessage(msg *ProducerMessage, err error) {
	if msg.retries >= p.conf.Producer.Retry.Max {
		p.returnError(msg, err)
	} else {
		msg.retries++
		p.retries <- msg
	}
}

func (p *asyncProducer) retryMessages(batch []*ProducerMessage, err error) {
	for _, msg := range batch {
		p.retryMessage(msg, err)
	}
}

func (p *asyncProducer) getBrokerProducer(broker *Broker) *brokerProducer {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	bp := p.brokers[broker]

	if bp == nil {
		bp = p.newBrokerProducer(broker)
		p.brokers[broker] = bp
		p.brokerRefs[bp] = 0
	}

	p.brokerRefs[bp]++

	return bp
}

func (p *asyncProducer) unrefBrokerProducer(broker *Broker, bp *brokerProducer) {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	p.brokerRefs[bp]--
	if p.brokerRefs[bp] == 0 {
		close(bp.input)
		delete(p.brokerRefs, bp)

		if p.brokers[broker] == bp {
			delete(p.brokers, broker)
		}
	}
}

func (p *asyncProducer) abandonBrokerConnection(broker *Broker) {
	p.brokerLock.Lock()
	defer p.brokerLock.Unlock()

	bc, ok := p.brokers[broker]
	if ok && bc.abandoned != nil {
		close(bc.abandoned)
	}

	delete(p.brokers, broker)
}
