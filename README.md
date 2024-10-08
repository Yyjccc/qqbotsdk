## qqbot-sdk

- qq官方机器的sdk
- 参考官方sdk
- 删去频道相关api

迁移v2 版本 openapi

完成的主要接口：
（群聊/单聊）
1. 上传富文本信息
2. 回复富文本消息
3. 回复文本消息

添加对单聊和群聊的事件订阅

使用：
```go
    token := entry.BotToken(config.APPID, config.APP_TOKEN)
	api := NewSandboxOpenAPI(token).WithTimeout(3 * time.Second)
	ctx := context.Background()
	ws, err := api.WS(ctx, nil, "")
	if err != nil {
		log.Printf("%+v, err:%v", ws, err)
	}
	intent := websocket.RegisterHandlers( //DirectMessageHandler(),
		AloneMessage(ctx, api),
	)

	// 启动 session manager 进行 ws 连接的管理，如果接口返回需要启动多个 shard 的连接，这里也会自动启动多个
	e := NewSessionManager().Start(ws, token, &intent)
	if e != nil {
		log.Printf(e.Error())
	}

}

func AloneMessage(ctx context.Context, api openapi.OpenAPI) websocket.MessageHandler {
	return func(event *websocket.WSPayload, data *websocket.WSMessageData) error {
		wrapper := &send.RawMessageWrapper{
			Payload: event,
			Data:    data,
		}
		url := "https://img.nga.178.com/attachments/mon_202402/11/mqQ2t-4ea9K1sT3cSk8-hw.jpg"
		//raw, err := api.ReplyTextMessageByRaw(ctx, data.Content, wrapper)
		rae, err := api.ReplyMediaMessageByRae(ctx, url, send.ImageType, wrapper)
		if err != nil {
			util.Errorf(err.Error())
		}
		util.Infof("发送消息id: %v", rae.Id)
		return err
	}
}
```

由于基于官方sdk ，有些结构体可能重复