# RFC: 节点配对机制(naozhi node / join)

- 状态: Draft v1(待评审)
- 日期: 2026-06-19
- 范围: 新增 `naozhi node add|ls|rm`(primary 侧)与 `naozhi join <code>`(worker 侧)子命令,把"本机 worker + 服务器 primary"的 reverse-connect 拓扑从"两端手写两份必须严格对齐的 YAML + 手工搬运 token"降到"primary 一条命令生成配对码 → worker 粘贴一条命令完成连接"。借鉴 `tailscale up --authkey` / k3s join token 的成熟心智模型。

## 1. 背景与问题(Background & problem)

### 1.1 现状:三条安装路径,"结合"那一环最弱

naozhi 的单机安装链路已经相当干净:

- 装二进制:`curl install.sh | bash` → `~/.local/bin`,校验 SHA256、无 sudo(`install.sh`)。
- 自更新:`naozhi upgrade` + 后台自动更新(`cmd/naozhi/upgrade.go`)。
- 注册服务:`naozhi install`,Linux=systemd / macOS=launchd(`cmd/naozhi/service.go` / `service_systemd.go`)。
- 配凭据:`naozhi setup weixin` 扫码自动写 config(`cmd/naozhi/setup.go`)。
- 诊断:`naozhi doctor`(`cmd/naozhi/doctor.go`)。

但 naozhi 的杀手场景 —— **公网服务器 primary 接 IM/Dashboard、本机 worker 持有真实代码仓库与 claude 认证,worker 主动反连 primary(NAT 穿透)** —— 恰恰是安装体验最差的一环。

```
   用户发消息                    ┌─────────────────────────────┐
  (微信/飞书) ───────────────▶ │  primary (EC2)              │  公网稳定 · 接 IM · Dashboard
                               │  :8180  reverse_nodes:{...} │
                               └───────────┬─────────────────┘
                                           │  ▲ worker 主动拨号 wss://…/ws-node
                                           ▼  │   (register 帧带 node_id+token)
                               ┌─────────────────────────────┐
                               │  worker (本机 Mac)          │  真实仓库 · claude 认证 · 本地工具链
                               │  upstream:{url,node_id,...} │
                               └─────────────────────────────┘
```

### 1.2 痛点

要把这套跑起来,今天用户得手工完成:

1. 在 primary 的 `config.yaml` 写 `reverse_nodes.<id>.token`(`internal/config/config.go:69`,`reversenode.go:4-7`)。
2. 在 worker 的 `config.yaml` 写 `upstream.{url,node_id,token}`(`config.go:148-154`)。
3. **保证两份配置里的 token 逐字节一致、node_id 一致** —— 无校验、无引导,错了只能翻日志(worker 侧 `slog.Warn("connector disconnected")`,primary 侧 `slog.Warn("reverse node auth failed")`,`reverseserver.go:379`)。
4. 把 token 安全地从 primary 搬到 worker。
5. 两端各自重启服务。

token 即身份(primary 侧只按 token 值匹配,同 token 的多个 node_id 可互换,`reverseserver.go:261` 注释),且 token 无最小熵要求(`config.go:1022` 仅校验非空+无未展开 `${VAR}`)—— 新手既容易配错,也容易配出弱 token。这是整条安装链里最劝退的一环,而它本应是 naozhi 最大的差异化能力。

## 2. 目标与非目标(Goals & non-goals)

### 目标
- **primary 侧**:`naozhi node add <id>` 一条命令生成 reverse-node 槽位 + 高熵随机 token,并打印一个**自包含配对码**;`node ls` / `node rm` 管理。
- **worker 侧**:`naozhi join <code>` 粘贴即用 —— 解码配对码 → 写 `upstream:` 块 → 注册本机服务并启动 → 验证连上。
- token 由机器生成(高熵),用户不手敲、不手抄字段。
- 全程复用现有的安全写 config 路径(`yaml.Node` + `DoubleQuotedStyle`)与服务注册路径(`runInstall`),不引入第二套架构。

### 非目标
- **不改 reverse-connect 的 wire 协议**(register/registered 帧、token 走第一帧、SHA-256 + 常数时间比较 `reverseserver.go:346-365`)。配对只是"把正确的值填到正确的位置"的编排层。
- **不做 forward/pull 模式(`nodes:` / `internal/node/relay.go`)的配对** —— 那条路径需要 primary 能反向 HTTP 直达 worker,与"本机在 NAT 后"的核心场景相悖。MVP 只覆盖 reverse 模式。
- **不引入中心化的注册服务器 / 账号体系**。配对码是 primary 与 worker 之间的点对点凭据,不经第三方。
- **不替代 `naozhi install`**;join 在内部复用它,不是平行实现。
- **不做 `naozhi init` 角色向导 / install.sh 衔接 / 凭据统一**(原分析的方案 B/C/D)。它们都能在本 RFC 之上增量叠加,见 §12 相关工作,本 RFC 不实现。

## 3. 设计概览(Design overview)

### 3.1 端到端流程

```
# ── primary (EC2) ──────────────────────────────────────────────
$ sudo naozhi node add macbook --url wss://naozhi.example.com/ws-node
  ✓ 已生成 reverse_nodes.macbook(token 32B,base64)
  ✓ 已写入 /home/ec2-user/.naozhi/config.yaml
  ⚠ 新节点需重启 primary 生效:sudo systemctl restart naozhi

  在 worker 机器上执行(配对码含密钥,请通过安全渠道传输):

      naozhi join nzj_eyJ1Ijoid3NzOi8vbmFvemhpL…

# ── worker (本机 Mac) ──────────────────────────────────────────
$ naozhi join nzj_eyJ1Ijoid3NzOi8vbmFvemhpL…
  ✓ 配对码解析:primary=wss://naozhi.example.com/ws-node node_id=macbook
  ✓ 已写入 upstream 配置 ~/.naozhi/config.yaml
  ✓ 已注册 launchd agent 并启动
  ⏳ 等待连接 primary…
  ✓ 已连上(node_id=macbook,backoff 回落 1s)
```

### 3.2 配对码(pairing code)格式

```
nzj_<base64url-nopad(JSON)>

JSON = {
  "u": "wss://naozhi.example.com/ws-node",  // upstream.url
  "n": "macbook",                            // upstream.node_id
  "t": "<32B base64 token>",                 // upstream.token / reverse_nodes[n].token
  "v": 1,                                    // 配对码 schema 版本
  "k": false                                 // 可选: insecure(ws:// 明文,默认 false)
}
```

- 前缀 `nzj_` 便于一眼识别与 grep。
- base64url-nopad:避开 shell/URL 特殊字符,可安全地作为单个命令行参数粘贴。
- `v` 版本字段:未来若改字段名/语义,worker 端可拒绝不认识的版本并提示升级。
- **node_id 由 primary 权威指定**(primary 是 reverse_nodes 这张表的归属方),worker 直接采用,避免"两端各填一个、对不上"。

### 3.3 命令矩阵

| 命令 | 运行在 | 作用 |
|---|---|---|
| `naozhi node add <id> [--url U] [--display-name N]` | primary | 生成 token、写 `reverse_nodes.<id>`、打印配对码 |
| `naozhi node ls` | primary | 列出已配置的 reverse_nodes(token 脱敏)+ 实时连接状态(best-effort) |
| `naozhi node rm <id>` | primary | 删除 `reverse_nodes.<id>` |
| `naozhi join <code> [--no-service] [--config P]` | worker | 解码 → 写 `upstream:` → 注册服务 → 验证连接 |

二级子命令模式(`node` 下再 switch `add/ls/rm`)沿用现有 `setup weixin` / `shim run|stop|list` 的惯例(`setup.go:94`,`shim.go`)。

## 4. 命令详细设计

### 4.1 `naozhi node add <id>`(primary)

流程:
1. `flag.NewFlagSet("node add", flag.ExitOnError)`,flag:`--url`(primary 的对外 wss URL)、`--display-name`、`--config`(默认 `~/.naozhi/config.yaml`,照抄 `service.go:94-97` 的展开)。
2. **解析 primary URL**(见 §4.5 的 URL 来源讨论)。校验必须是 `wss://`(或 `ws://` 且显式 `--insecure`),path 默认补 `/ws-node`。
3. **生成 token**:复用 `internal/shim/state.go:162` 的 `GenerateToken()`(32B crypto/rand → base64.Std),或在 `internal/cryptoutil` 加一个 N 字节版本(参考 `cron/job.go:355-395` 的 `io.ReadFull(rand.Reader,…)` 写法以规避 Go 1.26 entropy 行为)。**不复用 16B 的 `RandomCookieGen`**(熵偏低且失败即 panic)。
4. **写 config**:`reverse_nodes` 是 map-of-maps,用现有 `yamlFindOrCreateMap`/`yamlSetScalar` 组合(`setup.go:325/341`):
   ```
   rn    := yamlFindOrCreateMap(root, "reverse_nodes")
   entry := yamlFindOrCreateMap(rn, id)
   yamlSetScalar(entry, "token", token)              // DoubleQuotedStyle 防注入
   yamlSetScalar(entry, "display_name", displayName)
   ```
   经 `osutil.WriteFileAtomic(path, …, 0600)` 原子落盘(`setup.go:295`),目录 `0700`(`setup.go:271`)。**禁止字符串拼接**(沿用 `setup.go:61-67` 对 token-into-config 的强约束)。
5. **id 已存在的处理**:默认拒绝并提示用 `--rotate` 显式轮换 token(防误覆盖已在用的节点)。
6. **打印配对码** + 安全传输警告 + "需重启 primary 生效"提示(见 §7.1)。

### 4.2 `naozhi join <code>`(worker)

流程:
1. 解析 `nzj_` 前缀 → base64url decode → JSON unmarshal → 校验 `v`、必填字段、URL scheme。
2. **写 config**:`upstream` 是扁平 scalar 映射,直接 `yamlFindOrCreateMap(root,"upstream")` + 逐字段 `yamlSetScalar`。若 worker 此前无 config,用一份最小模板播种(类比 `setup.go:defaultConfigTemplate`,只含 `cli.path` 默认 + 空 `upstream`),其余字段交给 `config.applyDefaults`。
3. **写后校验**:调 `config.Load(path)`(`config.go:577`)做一次完整校验 —— 它会验证 `upstream.url` 是 `ws://`/`wss://`、`ws://` 必须 `insecure:true`(`config.go:990-1018`)、token 非空非占位。校验失败则报错并**不注册服务**(避免把坏 config 喂给 launchd/systemd 反复重启)。这是 `setupWriteConfig` 当前**没有**做的一步,join 必须补上。
4. **注册服务**:复用 `runInstall(["-config", absPath])` 整体(`service.go:80`),自动获得 binary 解析(`os.Executable()`+`EvalSymlinks`)、OS 分派、systemd root 检查。`--no-service` 跳过(仅写 config,适合用户已自管进程/容器)。
   - macOS worker(最常见的"本机")→ launchd,无需 root,契合度最高。
   - Linux worker → systemd,需 `sudo naozhi join`。文档须写明。
5. **验证连接**(best-effort,可 `--no-wait` 跳过):服务起来后,等待至多 ~15s,通过下列信号判定连上:
   - 首选:worker 日志出现 `slog.Info("connected to primary", …)`(`upstream/connector.go:418`);
   - 数值信号:`naozhi_upstream_connector_backoff_millis`(`upstream/reqsem_metrics.go:71`)回落到 1000ms 表示稳态(`connector.go:271`);未连上时持续高位。
   - 验证超时只 warn 不 fail(连接是异步重连的,primary 可能还没重启)。

### 4.3 `naozhi node ls`(primary)

读 config 的 `reverse_nodes`,逐行打印 `<id>  <display_name>  token=<脱敏>`。token 脱敏沿用 `chatIDSuffix` 风格(`main_helpers.go:165`,只留尾 8 位前缀 `…`)。

实时连接状态为可选增强:若 primary 在线且配了 `dashboard_token`,可经 loopback 调一个已有 API 拉当前已注册节点列表叠加显示;离线时只列静态配置。MVP 可先只做静态列表。

### 4.4 `naozhi node rm <id>`(primary)

从 config 删除 `reverse_nodes.<id>`。注意:现有 `yaml.Node` 工具箱**只有 find-or-create,没有删除 key 的 helper** —— 需新增一个 `yamlDeleteKey(parent, key)`(在 `parent.Content` 里定位 key/value 对并切片删除)。删除后同样提示需重启 primary。

### 4.5 关键设计约束:primary URL 从哪来

**naozhi 当前没有 `server.external_url` / `public_url` 配置字段**(已核实:`internal/config/` 无此项,只有 `server.addr` 监听地址 `config.go:184`)。primary 无法自知其公网可达 URL。三种处理:

- **MVP(选定)**:`node add` 要求显式 `--url wss://host/ws-node`。缺失则交互提示输入。诚实、零猜测。
- **增强(本 RFC 附带,可选)**:新增可选 config 字段 `server.external_url`。配了之后 `node add` 自动填充、可省略 `--url`。这是个小而独立的增量,放 §11 PR3。
- **不做**:从 `server.addr` + 网卡 IP 猜测公网 URL —— EC2/NAT/CloudFront 多层下几乎必猜错,反而制造"看起来对其实连不上"的坑。

## 5. 安全模型(Security model)

### 5.1 配对码含明文 token —— 这是核心权衡

配对码内嵌 32B token 明文(base64)。任何拿到配对码的人都能把自己注册成该 node_id(token 即身份)。因此:

- **MVP**:配对码定位为**敏感凭据**,等同 SSH 私钥/authkey。命令输出**必须**带显式警告("含密钥,请通过安全渠道传输,勿贴公开渠道/截图")。这与 tailscale `--authkey`、k3s `K3S_TOKEN` 的现状一致 —— 它们也是明文 join token。
- **传输**:由用户负责(已有安全信道:同机复制、加密 IM、密码管理器)。naozhi 不做密钥分发。

### 5.2 进阶:短时效 pairing 端点(本 RFC 不实现,列为 follow-up)

更稳妥的迭代(类似 tailscale 的 ephemeral authkey / OAuth):

- `node add` 不直接吐 token,而在 primary 开一个**短时效(如 10 分钟)、一次性**的 pairing 端点,返回一个**6 位短码**;
- worker `join <6位码>` 时向 primary 的 pairing 端点用短码换取真实 token + 配置,换完即焚。

这样泄露窗口从"永久"缩到"分钟级 + 一次性",且短码本身不是长期凭据。代价是要在 server 侧新增一个带 TTL 的 pairing handler + 状态。**列为 §12 follow-up**,MVP 先用自包含 base64 配对码跑通端到端。

### 5.3 传输层

- 配对码默认 `wss://`(TLS)。token 在 reverse 连接里走**第一帧明文 JSON**(`connector.go:386-396`),安全完全依赖 wss —— 这是既有协议事实,不是本 RFC 引入。
- `ws://`(明文)必须配对码显式带 `k:true` 且 worker config 写 `insecure:true`,否则 `config.Load` 拒绝(`config.go:1000`)。primary 侧对公网明文 upgrade 也会 403(`reverseserver.go:136-141`)。仅供同机/可信私网测试。

### 5.4 token 强度

`node add` 生成 32B crypto/rand token,远高于当前 config 校验的"仅非空"下限(`config.go:1022`)。这顺带把"用户手敲弱 token"的常见坑堵死。

### 5.5 终端注入防护

配对码解码出的字段写入 config 前,全部经 `yamlSetScalar` 的 `DoubleQuotedStyle`(`setup.go:341-356`),YAML 特殊字符无法注入伪造键。打印配对码到终端时,字段均为自生成(token=base64、url 经 scheme 校验),无外部不可信字节;若未来 §5.2 的 primary 返回体进入终端,须经 `osutil.SanitizeForLog`(`setup.go:203` 的先例)防 ANSI 转义劫持。

## 6. 关键代码锚点与复用(Code anchors)

| 能力 | 复用点 | 位置 |
|---|---|---|
| 子命令分发 | switch 加 `case "node"` / `case "join"` | `cmd/naozhi/main.go:43-66` |
| handler 命名 | `runNode(args []string)` / `runJoin(args []string)` | 同 `runSetup` 等 |
| flag 解析 | `flag.NewFlagSet(name, flag.ExitOnError)` | `setup.go:104`,`service.go:81` |
| 随机 token | `shim.GenerateToken()`(32B→base64) | `internal/shim/state.go:162` |
| 写 config(map-of-maps) | `yamlFindOrCreateMap`+`yamlSetScalar` | `setup.go:325/341` |
| 原子写 + 0600 | `osutil.WriteFileAtomic` | `internal/osutil/atomicfile.go:46` |
| 写后校验 | `config.Load(path)` | `internal/config/config.go:577` |
| 服务注册 | `runInstall(args)` 整体 | `cmd/naozhi/service.go:80` |
| 读现有 token | `loadTokenBestEffort()` | `cmd/naozhi/doctor.go:124` |
| token 脱敏展示 | `chatIDSuffix` 风格 | `cmd/naozhi/main_helpers.go:165` |
| 连接成功信号 | `connected to primary` 日志 / backoff metric | `upstream/connector.go:418`,`reqsem_metrics.go:71` |
| reverse-node 结构 | `ReverseNodeEntry{Token,DisplayName}` | `internal/config/reversenode.go:4-7` |
| upstream 结构 | `UpstreamConfig{URL,NodeID,Token,DisplayName,Insecure}` | `internal/config/config.go:148-154` |

**需新增的小工具**:`yamlDeleteKey(parent *yaml.Node, key string)`(`node rm` 用,工具箱当前缺删除原语)。

## 7. 落地约束(Operational constraints)

### 7.1 reverse_nodes 是启动时加载,新增 node 需重启 primary —— 关键约束

primary 的 token 校验表在启动时一次性构造:`buildReverseNodeAuth(cfg)` → `node.NewReverseServer(...)`(`cmd/naozhi/main.go:383-385`,`main_init.go:116-122`)。**`node add` 写完 config 后,新 token 不会自动生效,必须重启 primary。**

MVP 处理:`node add` 输出末尾明确打印 `⚠ 需重启 primary:sudo systemctl restart naozhi`(或 `naozhi install` 在 unit 不变时也会触发 restart)。

Follow-up(§12):给 `ReverseServer` 加 `UpdateAuth(map)` 热重载 + SIGHUP/API 触发,使 `node add` 真正即时生效,无需重启。这影响"零停机加节点"体验,但需评审热重载对在连节点的影响,本 RFC 不做。

### 7.2 worker 的最小 config

worker 节点是 stock naozhi,消息从 primary 经 RPC 流入(`connector_rpc.go`),**不需要 IM 平台凭据**。join 写的最小 config 只需:`cli.path`(默认 `claude`)+ `upstream:` 块,其余由 `applyDefaults` 填充。worker 自己也跑 `:8180`,是否给 worker 配 `dashboard_token`(让用户也能直连 worker 自己的 dashboard)留作可选,MVP 不强制。

### 7.3 join 在已有 config 上的幂等性

`yaml.Node` round-trip 会**剥除已有文件的注释**(整文档重编码,`setup.go` 已有此行为)。join 覆盖 `upstream` 块时,用户原 config 的注释会丢失。MVP 接受此行为(与 `setup weixin` 一致),文档注明;重复 `join` 同一 code 应幂等(覆盖为相同值)。

## 8. 备选方案(Alternatives considered)

### 方案 A(本 RFC 选定)— 自包含 base64 配对码 + 本地命令编排
primary 生成完整配对码,worker 本地解码并自配。无新增网络端点。
- 优点:零新增服务端面、端到端最短、纯本地、可离线生成、与 tailscale/k3s 心智一致。
- 代价:配对码含长期明文 token,泄露窗口=永久(缓解:§5.1 警告 + §5.2 follow-up)。

### 方案 B — primary 短时效 pairing 端点 + 6 位短码
见 §5.2。
- 优点:泄露窗口分钟级 + 一次性,短码非长期凭据,体验更接近 tailscale。
- 缺点:需 server 侧新增带 TTL/状态的 handler + 鉴权,面更大。**作为 A 的演进,不是替代**。

### 方案 C — 现状(手写两份对齐 YAML)
- 唯一优点:零新代码。缺点见 §1.2,正是本 RFC 要解决的。

### 为何选 A
A 是能独立交付、ROI 最高、改动面最可控的 MVP:几乎全部站在既有的 `yaml.Node` 写入、`runInstall`、`shim.GenerateToken` 之上,不动 wire 协议、不加服务端状态。B 的安全收益真实,但应在 A 跑通后作为增量叠加(§12),避免一上来就背 server 侧 TTL 状态的复杂度。

## 9. 测试策略(Test strategy)

### 9.1 单元测试(纯函数优先)
- **配对码编解码**:`encodePairingCode(payload)` / `decodePairingCode(string)` 往返测试;拒绝缺前缀、坏 base64、未知 `v`、缺必填字段、非 `ws/wss` scheme。模糊测试(fuzz)解码器防 panic。
- **`yamlDeleteKey`**:删除存在/不存在的 key、删除后文档可被 `config.Load` 解析、保留其余键。
- **写 reverse_nodes / upstream 块**:给定空 config / 已有 config,断言写出的 YAML 经 `config.Load` 校验通过且 token 走 DoubleQuotedStyle(含 YAML 特殊字符的 token 不破坏文档 —— 复用 `setup.go` 的 `updateWeixinToken` 测试范式)。
- **token 生成**:长度=32B、两次调用不相等、base64 可解码。

### 9.2 校验门槛
- join 写出的 config 必须通过 `config.Load`(`config.go:990-1018` 的 upstream 校验):`wss://` 放行、`ws://` 无 insecure 被拒、占位 token 被拒。
- `node add` 写出的 config 必须通过 `reverse_nodes` 校验(`config.go:1020-1027`)。

### 9.3 集成 / 手动验证(端到端配对)
本机起两个实例验证闭环(注意 memory:正式实例 :8180 别碰,用独立端口):
1. primary 实例 A(`server.addr: :18180`)`node add macbook --url ws://127.0.0.1:18180/ws-node --insecure` → 拿配对码 → 重启 A。
2. worker 实例 B `join <code> --no-service`(本机测试不注册 launchd,手动起 B)→ 确认 B config 写对。
3. 起 B → 断言:B 日志 `connected to primary`、A 日志 `reverse node registered`(`reverseserver.go:463`)、`naozhi_upstream_connector_backoff_millis` 回落 1000。
4. 负路径:篡改 worker token → A 日志 `reverse node auth failed`、B 持续重连 backoff 高位。

### 9.4 全量回归
`go build ./...` / `go test -race ./...` / `go vet ./...` 全绿(注意 memory 记录的两个 pre-existing 失败 TestLegacyServerNew、sessionRuns lint 非本 RFC 引入,别误判)。

## 10. 风险与回滚(Risk & rollback)

| 风险 | 缓解 |
|---|---|
| 配对码泄露 = 永久 node 身份被冒用 | §5.1 显式警告;§5.2 follow-up 短时效端点;`node rm` + rotate 可吊销 |
| 用户忘记重启 primary,配对"看起来没成功" | §7.1 `node add` 末尾强提示;join 验证超时 warn 文案提示"primary 是否已重启" |
| join 写坏 config 导致服务反复重启 | §4.2 步骤 3 写后 `config.Load` 校验,失败不注册服务 |
| Linux worker join 需 sudo 但用户没加 | join 检测到 systemd 分派且非 root 时,复用 `installSystemd` 的 `fatalf("… requires root")` 提示(`service_systemd.go:189`) |
| `yaml.Node` round-trip 丢注释 | §7.3 文档注明,与现有 `setup weixin` 行为一致 |

**回滚**:全部为新增子命令(`node`/`join`)+ 一个新 yaml helper + 可选的 `server.external_url` 字段,不改任何既有路径。回滚 = revert 对应 commit,既有 install/setup/reverse 链路零影响。非 flag-gated(新命令默认不被任何现有流程调用)。

## 11. 落地计划(Rollout plan)

**PR1 — 配对码 + worker join(端到端最小闭环)**
1. `internal/pairing`(新叶子包):`Payload` 结构 + `Encode`/`Decode`(base64url + JSON + 版本校验),零外部依赖,全单测。
2. `cmd/naozhi/join.go`:`runJoin` —— 解码 → 写 `upstream:`(yaml.Node)→ `config.Load` 校验 → `runInstall` 注册 → 验证连接。`--no-service`/`--no-wait`/`--config` flag。
3. `cmd/naozhi/main.go:43` 加 `case "join"`。
4. 测试:9.1 配对码往返 + 9.2 校验门槛 + join 写 upstream 块。

**PR2 — primary node 子命令**
1. `cmd/naozhi/node.go`:`runNode` 二级分发 `add`/`ls`/`rm`。
2. token 生成复用 `shim.GenerateToken()`;写 `reverse_nodes`(yaml.Node);打印配对码 + §5.1 警告 + §7.1 重启提示。
3. 新增 `yamlDeleteKey`(供 `node rm`)。
4. `main.go` 加 `case "node"`。
5. 测试:9.1 `yamlDeleteKey` + 写 reverse_nodes 块 + token 属性。

**PR3(可选)— `server.external_url` 自动填充 URL**
- `internal/config` 加可选 `server.external_url` 字段 + 校验(必须 ws/wss);`node add` 在 `--url` 缺省时读取它。纯增量,缺省不影响任何行为。

**PR4(可选,见 §7.1)— reverse_nodes 热重载**
- `ReverseServer.UpdateAuth` + 触发机制,使 `node add` 免重启。需单独评审热重载语义。

**文档**:README §部署 增"节点配对"小节(替代手写 YAML 的示例);`docs/ops/` 增配对故障排查(对照 §9.3 的日志/metric 信号)。

## 12. 相关工作与后续(Related work)

本 RFC 聚焦"配对机制 MVP"。原安装体验分析中的其余方向,均可在此之上增量叠加,**不在本 RFC 范围**:

- **短时效 pairing 端点**(§5.2):把配对码从"永久明文 token"升级为"分钟级一次性短码"。安全收益最高的下一步。
- **`naozhi init` 角色向导**:交互式三选一(本地单机 / primary 网关 / worker 节点),worker 分支直接进 `join` 流程;并提供非交互 `--role=worker --join=<code>` 供 CI。把 setup/install/join 统一到一个入口。
- **install.sh 衔接**:装完二进制提示 `naozhi init`;支持 `NAOZHI_ROLE=worker NAOZHI_JOIN=<code> curl … | bash` 一行装成 worker。
- **凭据统一**:把 `setup` 扩到 feishu/slack/discord,消除"weixin 走 config、其余走 ~/.naozhi/env"的割裂。

## 13. 开放问题(Open questions)

1. **node_id 命名空间**:`node add` 是否校验 id 字符集(避免奇怪字符进 config key / 日志)?建议复用 `service.go:44-48` 的 username 字符校验风格(`[A-Za-z0-9_-]`)。
2. **node ls 实时状态**:MVP 静态列表是否够用,还是首版就要叠加在线状态(需 loopback 调 API + token)?
3. **rotate 语义**:`node add <已存在id> --rotate` 换 token 后,旧 worker 会鉴权失败重连 —— 是否需要 `node rotate` 独立命令以示区别?
4. **worker 反向也要 dashboard_token 吗**:worker 自己的 `:8180` 是否默认生成一个 token,还是留空(本机自用免鉴权)?涉及 §7.2。
5. **配对码是否带 primary 指纹**:是否在配对码里加 primary 的 TLS 证书指纹,让 worker 首连即可 pin,防 MITM?(增强,可能过度。)
