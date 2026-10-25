# 微信渠道测试指南

## 前提

- 不需要安装 OpenClaw
- 一个微信号就够：扫码的号自己就是测试号。登录响应里的 `ilink_user_id` 就是这个号发消息时的
  `from_user_id`，把它加进 `im_access` 的白名单后，用这个号给 bot 发消息即可

## 第一步：扫码登录（推荐 `naozhi setup weixin`）

```bash
bin/naozhi setup weixin --config config.yaml
```

用微信扫终端里打印的链接并在手机上确认。setup 会把 `bot_token` 写进
`platforms.weixin.token`，并把扫码用户的 `ilink_user_id` 写进
`im_access.platforms.weixin.allowed_users`（见 README「IM 访问控制」）。扫码过程中：

- 手机上显示一串数字时（`need_verifycode`），按提示把数字输进终端；输错会让你重输，错太多次
  （`verify_code_blocked`）setup 会退出，等一会儿再重跑
- 扫码用户在其他机房时（`scaned_but_redirect`），setup 会自动换到响应里的 `redirect_host` 继续轮询；
  登录响应里的 `baseurl` 不是默认地址时，setup 会把它写进 `platforms.weixin.base_url`

### 手动获取 Token（可选）

```bash
# 1. 获取二维码
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_bot_qrcode?bot_type=3"
# 返回 {"qrcode":"xxx","qrcode_img_content":"https://liteapp.weixin.qq.com/q/...","ret":0}

# 2. 用微信扫 qrcode_img_content 里的链接，在手机上确认

# 3. 轮询登录状态（把 QRCODE 换成第 1 步返回的 qrcode 值）
curl -s "https://ilinkai.weixin.qq.com/ilink/bot/get_qrcode_status?qrcode=QRCODE" \
  -H "iLink-App-ClientVersion: 1"
# 扫码确认后返回 {"status":"confirmed","bot_token":"xxx","ilink_bot_id":"xxx","ilink_user_id":"xxx@im.wechat",...}
# 返回 need_verifycode 时，在 URL 后加 &verify_code=<手机上显示的数字> 再轮询；
# 返回 scaned_but_redirect 时，把域名换成响应里的 redirect_host 再轮询

# 4. 记下 bot_token 和 ilink_user_id；baseurl 不是 https://ilinkai.weixin.qq.com 时也记下，填进 base_url
```

## 第二步：配置 config.yaml

用 `naozhi setup weixin` 时这一步已经自动完成，检查一下即可。手动配置：

```yaml
platforms:
  weixin:
    token: ${WEIXIN_BOT_TOKEN}   # 或直接填 bot_token
    # base_url: https://ilinkai.weixin.qq.com  # 默认值，可省略

im_access:
  platforms:
    weixin:
      allowed_users: ["xxx@im.wechat"]   # 登录响应里的 ilink_user_id
```

不写 `im_access` 时，所有能给 bot 发消息的人都能在宿主机上跑命令。没拿到 `ilink_user_id`
也没关系：先只写 `im_access: {default_deny: true}`（没有条目的平台一律拒绝，名单不能写空列表，
config 校验会报错），发一条消息后 bot 会在私聊里回复你的 ID（日志里也有一行 `im access denied`），
再按上面的格式填进去并 `naozhi config reload`。

## 第三步：启动

```bash
export WEIXIN_BOT_TOKEN="上面拿到的bot_token"   # 用 setup 写入的配置时不需要
bin/naozhi --config config.yaml
```

## 第四步：发消息测试

用扫码的微信号给 bot 发消息，naozhi 会通过 Claude CLI 处理后回复。其他微信号发来的消息会被拒绝，
对方会收到一条带自己 ID 的提示，要放行就把 ID 加进 `allowed_users`。

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

## 踩坑记录：多轮对话必须携带 base_info

### 问题

使用 iLink Bot API 时，第一条 `sendMessage` 能正常送达微信，但第二条及之后的消息全部静默丢弃（API 返回 `{}` 无错误）。

### 根因

iLink Bot API 要求每个请求体携带 `base_info` 字段：

```json
{
  "msg": { ... },
  "base_info": { "channel_version": "naozhi-1.0.0" }
}
```

缺少 `base_info` 时，iLink 服务端将客户端视为非标准接入，**降级为一次性回复模式**——仅允许每次扫码后发送一条消息。

### 发现过程

通过分析腾讯官方 OpenClaw 微信插件 `@tencent-weixin/openclaw-weixin`（npm 包）的源码发现。该插件也使用同样的 HTTP API（`getupdates` / `sendmessage`），但每个请求都带了 `base_info: { channel_version }`。

### 完整修复清单

| 修复项 | 说明 |
|--------|------|
| `base_info.channel_version` | 每个请求体必须携带，标识客户端版本 |
| `msg.client_id` | sendMessage 时带唯一请求 ID |
| `msg.from_user_id` | 设为空字符串 `""`，不是 bot ID |
| `ilink/bot/getconfig` | 新 API：获取 `typing_ticket` |
| `ilink/bot/sendtyping` | 新 API：发送"正在输入"指示器 |

### 参考

- 官方插件包：`npm pack @tencent-weixin/openclaw-weixin`
- 安装器：`npx -y @tencent-weixin/openclaw-weixin-cli@latest install`
- iLink Bot API 无公开文档，以上均通过逆向插件源码获得

