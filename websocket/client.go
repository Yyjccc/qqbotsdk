package websocket

import (
	"encoding/json"
	"fmt"
	"github.com/Yyjccc/qqbotsdk/util"
	wss "github.com/gorilla/websocket" // 是一个流行的 websocket 客户端，服务端实现

	"os"
	"os/signal"
	"syscall"
	"time"
)

// DefaultQueueSize 监听队列的缓冲长度
const DefaultQueueSize = 10000

// Setup 依赖注册
func Setup() {
	RegisterWS(&Client{})
}

// New 新建一个连接对象
func (c *Client) New(session Session) WebSocket {
	return &Client{
		messageQueue:    make(messageChan, DefaultQueueSize),
		session:         &session,
		closeChan:       make(closeErrorChan, 10),
		heartBeatTicker: time.NewTicker(60 * time.Second), // 先给一个默认 ticker，在收到 hello 包之后，会 reset
	}
}

// Client websocket 连接客户端
type Client struct {
	version         int
	conn            *wss.Conn
	messageQueue    messageChan
	session         *Session
	user            *WSUser
	closeChan       closeErrorChan
	heartBeatTicker *time.Ticker // 用于维持定时心跳
}

type messageChan chan *WSPayload
type closeErrorChan chan error

// Connect 连接到 websocket
func (c *Client) Connect() error {
	if c.session.URL == "" {
		return util.ErrURLInvalid
	}

	var err error
	c.conn, _, err = wss.DefaultDialer.Dial(c.session.URL, nil)
	if err != nil {
		util.Errorf("%s, connect err: %v", c.session, err)
		return err
	}
	util.Infof("%s, url %s, connected", c.session, c.session.URL)

	return nil
}

// Listening 开始监听，会阻塞进程，内部会从事件队列不断的读取事件，解析后投递到注册的 event handler，如果读取消息过程中发生错误，会循环
// 定时心跳也在这里维护
func (c *Client) Listening() error {
	defer c.Close()
	// reading message
	go c.readMessageToQueue()
	// read message from queue and handle,in goroutine to avoid business logic block closeChan and heartBeatTicker
	go c.listenMessageAndHandle()

	// 接收 resume signal
	resumeSignal := make(chan os.Signal, 1)
	if ResumeSignal >= syscall.SIGHUP {
		signal.Notify(resumeSignal, ResumeSignal)
	}

	// handler message
	for {
		select {
		case <-resumeSignal: // 使用信号量控制连接立即重连
			util.Infof("%s, received resumeSignal signal", c.session)
			return util.ErrNeedReConnect
		case err := <-c.closeChan:
			// 关闭连接的错误码 https://bot.q.qq.com/wiki/develop/api/gateway/error/error.html
			util.Errorf("%s Listening stop. err is %v", c.session, err)
			// 不能够 identify 的错误
			if wss.IsCloseError(err, 4914, 4915) {
				err = util.New(util.CodeConnCloseCantIdentify, err.Error())
			}
			// 这里用 UnexpectedCloseError，如果有需要排除在外的 close error code，可以补充在第二个参数上
			// 4009: session time out, 发了 reconnect 之后马上关闭连接时候的错误码，这个是允许 resumeSignal 的
			if wss.IsUnexpectedCloseError(err, 4009) {
				err = util.New(util.CodeConnCloseCantResume, err.Error())
			}
			if DefaultHandlers.ErrorNotify != nil {
				// 通知到使用方错误
				DefaultHandlers.ErrorNotify(err)
			}
			return err
		case <-c.heartBeatTicker.C:
			util.Debugf("%s listened heartBeat", c.session)
			heartBeatEvent := &WSPayload{
				WSPayloadBase: WSPayloadBase{
					OPCode: WSHeartbeat,
				},
				Data: c.session.LastSeq,
			}
			// 不处理错误，Write 内部会处理，如果发生发包异常，会通知主协程退出
			_ = c.Write(heartBeatEvent)
		}
	}
}

// Write 往 ws 写入数据
func (c *Client) Write(message *WSPayload) error {
	m, _ := json.Marshal(message)
	util.Infof("%s write %s message, %v", c.session, OPMeans(message.OPCode), string(m))

	if err := c.conn.WriteMessage(wss.TextMessage, m); err != nil {
		util.Errorf("%s WriteMessage failed, %v", c.session, err)
		c.closeChan <- err
		return err
	}
	return nil
}

// Resume 重连
func (c *Client) Resume() error {
	payload := &WSPayload{
		Data: &WSResumeData{
			Token:     c.session.Token.GetString(),
			SessionID: c.session.ID,
			Seq:       c.session.LastSeq,
		},
	}
	payload.OPCode = WSResume // 内嵌结构体字段，单独赋值
	return c.Write(payload)
}

// Identify 对一个连接进行鉴权，并声明监听的 shard 信息
func (c *Client) Identify() error {
	// 避免传错 intent
	if c.session.Intent == 0 {
		c.session.Intent = IntentGuilds
	}
	payload := &WSPayload{
		Data: &WSIdentityData{
			Token:   c.session.Token.GetString(),
			Intents: c.session.Intent,
			Shard: []uint32{
				c.session.Shards.ShardID,
				c.session.Shards.ShardCount,
			},
		},
	}
	payload.OPCode = WSIdentity
	return c.Write(payload)
}

// Close 关闭连接
func (c *Client) Close() {
	if err := c.conn.Close(); err != nil {
		util.Errorf("%s, close conn err: %v", c.session, err)
	}
	c.heartBeatTicker.Stop()
}

// Session 获取client的session信息
func (c *Client) Session() *Session {
	return c.session
}

func (c *Client) readMessageToQueue() {
	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			util.Errorf("%s read message failed, %v, message %s", c.session, err, string(message))
			close(c.messageQueue)
			c.closeChan <- err
			return
		}
		payload := &WSPayload{}
		if err := json.Unmarshal(message, payload); err != nil {
			util.Errorf("%s json failed, %v", c.session, err)
			continue
		}
		payload.RawMessage = message
		util.Infof("%s receive %s message, %s", c.session, OPMeans(payload.OPCode), string(message))
		// 处理内置的一些事件，如果处理成功，则这个事件不再投递给业务
		if c.isHandleBuildIn(payload) {
			continue
		}
		c.messageQueue <- payload
	}
}

func (c *Client) listenMessageAndHandle() {
	defer func() {
		// panic，一般是由于业务自己实现的 handle 不完善导致
		// 打印日志后，关闭这个连接，进入重连流程
		if err := recover(); err != nil {
			PanicHandler(err, c.session)
			c.closeChan <- fmt.Errorf("panic: %v", err)
		}
	}()
	for payload := range c.messageQueue {
		c.saveSeq(payload.Seq)
		// ready 事件需要特殊处理
		if payload.Type == "READY" {
			c.readyHandler(payload)
			continue
		}
		// 解析具体事件，并投递给业务注册的 handler
		if err := ParseAndHandle(payload); err != nil {
			util.Errorf("%s parseAndHandle failed, %v", c.session, err)
		}
	}
	util.Infof("%s message queue is closed", c.session)
}

func (c *Client) saveSeq(seq uint32) {
	if seq > 0 {
		c.session.LastSeq = seq
	}
}

// isHandleBuildIn 内置的事件处理，处理那些不需要业务方处理的事件
// return true 的时候说明事件已经被处理了
func (c *Client) isHandleBuildIn(payload *WSPayload) bool {
	switch payload.OPCode {
	case WSHello: // 接收到 hello 后需要开始发心跳
		c.startHeartBeatTicker(payload.RawMessage)
	case WSHeartbeatAck: // 心跳 ack 不需要业务处理
	case WSReconnect: // 达到连接时长，需要重新连接，此时可以通过 resume 续传原连接上的事件
		c.closeChan <- util.ErrNeedReConnect
	case WSInvalidSession: // 无效的 sessionLog，需要重新鉴权
		c.closeChan <- util.ErrInvalidSession
	default:
		return false
	}
	return true
}

// startHeartBeatTicker 启动定时心跳
func (c *Client) startHeartBeatTicker(message []byte) {
	helloData := &WSHelloData{}
	if err := ParseData(message, helloData); err != nil {
		util.Errorf("%s hello data parse failed, %v, message %v", c.session, err, message)
	}
	// 根据 hello 的回包，重新设置心跳的定时器时间
	c.heartBeatTicker.Reset(time.Duration(helloData.HeartbeatInterval) * time.Millisecond)
}

// readyHandler 针对ready返回的处理，需要记录 sessionID 等相关信息
func (c *Client) readyHandler(payload *WSPayload) {
	readyData := &WSReadyData{}
	if err := ParseData(payload.RawMessage, readyData); err != nil {
		util.Errorf("%s parseReadyData failed, %v, message %v", c.session, err, payload.RawMessage)
	}
	c.version = readyData.Version
	// 基于 ready 事件，更新 session 信息
	c.session.ID = readyData.SessionID
	c.session.Shards.ShardID = readyData.Shard[0]
	c.session.Shards.ShardCount = readyData.Shard[1]
	c.user = &WSUser{
		ID:       readyData.User.ID,
		Username: readyData.User.Username,
		Bot:      readyData.User.Bot,
	}
	// 调用自定义的 ready 回调
	if DefaultHandlers.Ready != nil {
		DefaultHandlers.Ready(payload, readyData)
	}
}
