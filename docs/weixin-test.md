# 微信渠道测试指南

## 前提

- 不需要安装 OpenClaw
- 需要两个微信号（一个登录 bot，一个发消息测试）

## 第一步：扫码获取 Token

```bash
# 1. 获取二维码
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3"
# 返回 {"qrcode":"xxx","qrcode_img_content":"https://liteapp.weixin.qq.com/q/...","ret":0}

# 2. 用微信扫 qrcode_img_content 里的链接，在手机上确认

# 3. 轮询登录状态（把 QRCODE 换成第 1 步返回的 qrcode 值）
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_qrcode_status?qrcode=QRCODE" \
  -H "iLink-App-ClientVersion: 1"
# 扫码确认后返回 {"status":"confirmed","bot_token":"xxx","ilink_bot_id":"xxx",...}

# 4. 记下 bot_token
```

## 第二步：配置 config.yaml

```yaml
platforms:
  weixin:
    token: ${WEIXIN_BOT_TOKEN}   # 或直接填 bot_token
    # base_url: https://ilinkai.weixin.qq.com  # 默认值，可省略
```

## 第三步：启动

```bash
export WEIXIN_BOT_TOKEN="上面拿到的bot_token"
bin/naozhi --config config.yaml
```

## 第四步：发消息测试

用另一个微信号给扫码登录的微信号发消息，naozhi 会通过 Claude CLI 处理后回复。

## 验证清单

- [ ] getUpdates 长轮询正常（日志中有 `weixin platform started`）
- [ ] 收到用户消息（日志中有 session 创建）
- [ ] 回复消息送达微信（对方收到回复）
- [ ] `/new` 重置会话正常

## 技术说明

- 协议：iLink Bot API（`ilinkai.weixin.qq.com`），HTTP JSON 长轮询
- 收消息：`POST ilink/bot/getupdates`，35 秒长轮询
- 发消息：`POST ilink/bot/sendmessage`，需回传 `context_token`
- 认证：`Authorization: Bearer <bot_token>`，无需 App ID/Secret
- 连续失败 3 次自动退避 30 秒
