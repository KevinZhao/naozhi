# RFC: Workflow 运行进度看板（dashboard）

- 状态：Draft v4（评审第 3 轮修订）
- 日期：2026-10-05（v1）；2026-10-05（v2）；2026-10-05（v3）；2026-10-05（v4）
- 作者：Kevin Zhao
- 范围：naozhi 托管的 Claude Code session 通过 `Workflow` 工具（ultracode）跑的后台
  workflow，在 dashboard 上实时展示 header / phase / per-agent 进度、完成后的结果与
  日志、以及 drill-in 到每个 workflow agent 的 transcript；要求跨 naozhi 重启存活、
  parent session 空闲时照常更新
- 关联：`agent-team-ui.md`（Agent 工具子代理可视化 + drill-in，本 RFC 复用其
  TranscriptReader / tailer）、`event-log-persistence.md`（ring + persist）、
  `wsproto.md`（WS 协议单一真相源）、`kiro-effort-visibility.md`（`SessionSnapshot`
  透出字段的范式）
- 基线：worktree `/Users/zhaokm/workspace/naozhi-wfdash` @ `f08594f6`；CC 2.1.288
  （`~/.toolbox/tools/claude-code/2.1.288.1084/claude`）

## v4 变更摘要（vs v3）

评审第 3 轮的确认问题逐条对照代码复核，合并重复项（RunDir 同步解析被三位评审分别报告，`PathContainedInRoot` 参数顺序被三位评审报告，
超长行被两位评审报告）后为以下 26 项。评审给的修法不对或可以更简单的，按根因另解，在条目里注明。

**并发与生命周期**

1. **respawn 先携带、后绑定**（major；§5.8、§11.2、PR-8）。v3 让 bind 紧挨在 `installFreshSession` 内的 `bookProcessEnd`
   （`router_lifecycle.go:797-798`）之前，却在它返回之后才在 :671 处 `fresh.workflows = old.workflows`：新 proc 被绑到一个临时的空 board 上，
   随后这个 board 被旧指针覆盖。携带过来的 board 的 `b.proc` 是旧 proc 或 nil，永远收不到新 proc 的 wake，R4 还会把仍在跑的 workflow 标成 interrupted。
   现在 board 走 `respawnSnapshot`（`respawn_snapshot.go:60-104`，与 `codeChanges` 同一条路：`snapshotRespawn` 取、`rereadSameEntry` 重读），
   作为新参数传进 `installFreshSession`，在 `bookWorkflows` / `bookProcessEnd` 之前赋给 `s.workflows`；只有 `old == nil`（首次 spawn、`/new`）才新建。
   rename 本来就是先在 :78 一带携带、再在 :117 绑定，顺序正确，只是写明。新增测试：respawn 之后被绑定的 board 就是携带来的那个指针，且新 proc 的帧能到达它。
2. **RunDir 解析移出 `b.mu`、readLoop 与表事务**（major；§5.8、§8.1、§11.2、PR-8/9）。v3 让 board 在发布时同步调
   `ResolveWorkflowRunDir`，这个函数要做 `EvalSymlinks` 和结构检查，前缀不匹配时还会走 `sameFileAncestor` 的 Lstat 祖先遍历
   （`osutil/pathroot.go:33-58`）。发布发生在 `b.mu` 内、readLoop 上，bind / procEnded 还在 `r.ss.Update` 事务里，既违反 v3 自己的
   "磁盘 I/O 从不在 b.mu 内"，也超出 R2 的 readLoop 预算。只把调用挪到 wake 拿锁之前不够，因为 bind 本身就在事务里。现在发布只记一个
   "待解析"标记，连同来源元组和代数一起交给 board 的有界 I/O 派发（第 6 项）；解析在锁外完成，回到 `b.mu` 内复核 task、来源元组与代数都没变后才写
   RunDir，然后发布（RunDir 不上 wire，不推进 version）。解析完成之前，结果文件读取、R3、sweeper 跳过该条目，drill-in 返回 202 pending，
   HTTP 不返回 result / prompt。同时写明 board 从哪里拿 `projectsRoot`（创建时传入 `HistoryIO.projectsRoot`）和 workspace（每次 bind 传入
   `s.Workspace()`，ProjectDir 在解析任务里用 `claudefs.ProjectSlug` 纯字符串求出）。新增测试：用计数型 fake resolver 断言事务内 bind
   不做同步解析；resolver 卡住时 wake / bind 照常返回。
3. **board 的非终态上限只裁 retained**（§5.3、§5.8、PR-8）。v3 按 `LastObservedAt` 把最旧的非终态条目标 interrupted，却没排除 live 条目。
   而 Tracker 对第 17 个起的 running workflow 有意保留 header（`too_many`），live 本身就可能超过 16：每次 wake，进程侧优先把它改回 running，
   上限又把某一个标成 interrupted，被标的是哪一个还随 `LastObservedAt` 变化，于是状态翻来覆去、发出结构通知、误播终态。现在上限只作用于
   retained：live + retained 的非终态超过 16 时，按"unknown → snapshot_stale / Ref 来源 → 其余"的顺序、同类中 `LastObservedAt` 最旧的先删除
   （直接删，不合成 interrupted：被裁掉不能当作终态证据），直到满足上限或 retained 删空；live 条目从不因上限被裁，也从不被标成终态。
   Tracker 的 header-only 条目另设上限：live 非终态最多 32 个，超出的忽略并计数。Ref 仍 ≤ 21 条。
4. **有存活进程时，没人认领的 retained 条目也会收敛**（§5.6(6b)、§5.9 R5、PR-9）。R4 只在 board 没有存活进程时生效。重启后 shim 已死、
   90s 内又有消息 spawn 了新 CLI，`bind` 把 `procGone` 清零，于是 Ref 恢复出来的 running 条目再也没有规则能收敛它，每次存盘都以 running
   写回 Ref；adopt / paused 路径还不写结果文件。新增 R5：retained 的非终态条目从当前绑定起 `workflowUnclaimedAfter` 内都没被当前 Tracker
   报告过（replay 未绕回取 90s，已绕回取 10min，因为安静 phase 的帧可能已被挤出 ring），而且本轮 stat 也没找到 taskId 匹配的结果文件，就置为
   `unknown`（`RawStatus="unclaimed"`）。不置 interrupted，因为无法确认 CLI 已死；unknown 不钉保活（第 4 项 v3），仍是 sweeper 候选，Tracker
   之后若报告了同一 task_id 就以进程侧为准覆盖回来。`LastObservedAt` 超过 `workflowPinMax` 的 unknown 条目不再写进 Ref。
5. **重启后恢复的终态条目会补齐行**（§5.6(6b)、§5.9 R0、§6.2.1、PR-9）。Ref 不带 agents。sweeper 的终态重试候选要求 `now − EndedAt ≤ 10min`，
   R3b 又只在重接时执行，所以没有 shim 的重启之后，展开一个恢复出来的终态 workflow 只看得到 header。现在 R0 恢复的终态条目只要 RunID 已知，
   就在 RunDir 解析完成后做一次异步 `MergeResultFile`（行、总计、缓存一并合入，按普通 board 版本推进发布）；HTTP 的缓存缺失路径走同一个
   board 方法。复核时补上的一点：结果文件瘦结构里的 `workflowProgress[]` 原来只声明 `{type,index,agentId,state}`，由它建出来的行没有 label。
   现在它直接复用 `clievent.WorkflowItem`，和 stream 同形，同样不声明 preview，再加 `phases[].title`。
6. **后台磁盘 I/O 全局有界**（§5.6(6b)、§5.8、§5.3、PR-8/9）。v3 规定"在途超过 30s 视为卡死，允许发起新的读取"。在挂死的网络文件系统上，
   这会让每个 board 每 30s 泄漏一个 goroutine 和一个 fd，没有上限。现在 RunDir 解析、stat、结果文件读取、R3 都走 board 的同一个 I/O 派发：
   每个 board 最多 2 个在途任务，全局最多 `workflowIOSlots = 8` 个。在途超过 30s 的任务只标为卡死，**继续占着全局槽位**，每个 board 至多再补发
   1 个，直到卡死的任务返回为止。所以挂死的文件系统最多卡住 8 个 goroutine。

**解码与上限**

7. **超长行有天花板，写明并降级可见**（§1.2.3、§5.7、§13 R14、PR-7）。v3 说"不会截断"，只对实测过的大小成立。全量快照每个 agent 约 1.3KB，
   CJK 文本更多。大约 4k-7k 个 agent 时，一行就会超过 shim 的 10MB `bufio.Scanner` 上限：`readStdout` 的 `Scan()` 循环就此退出
   （`shim/server_cli.go:29-58`），shim 不再读取 CLI stdout，CLI 阻塞在管道上，session 卡死。稍小一点的行能过 shim，但转义后的 envelope 超过 naozhi 的
   10MiB 上限，会被静默跳过（`process_readloop.go:189-195`），而后续快照只会更大，面板冻结在最后一张放得下的快照上，看起来却一切正常。
   现在 §1.2.3 写明这个上限。readLoop 的 oversize 分支会窥视 envelope 前缀里转义过的 `subtype":"task_progress"` 与 task_id，命中已知 workflow 时
   置 `Degraded=snapshot_dropped`（chip："明细过大，已停止更新"）。shim 遇 `ErrTooLong` 结束 stdout 循环的问题属于 shim 既有行为，另开 issue，记为 R14。
8. **phases 有上限**（§5.3、§6.1、PR-6）。v3 只在 WS 帧上限 200 个 phase，Tracker、board 与 HTTP 都没有上限，而 `phase()` 由模型写的脚本调用。
   现在 Tracker 上限 200（按 index 升序保留，超出丢弃并计数，`Degraded=too_many`），HTTP 与 WS 自然继承。所属 phase 被丢弃的 agent 行在客户端归到
   "其余 phase"组。index 去重已由 §4.1.1 的身份复核保证。
9. **SeedFromReplay 跳过旧的小帧**（§5.9、§11.2、PR-6）。v3 只对"已有更新快照"的快照行跳过解码，可绕回后的 10000 行 ring 主要是小的 task_progress 行，
   每一行仍要反射解码。实测每行约 7-9µs，10k 行约 70-90ms、4.4MB，是 v3 所写"≤ ~20ms"的 3-4 倍。评审建议"三格都填满后跳过"，但 running 的 workflow
   没有终态帧，那一格永远填不满，修复对主场景无效。现在按 task、按帧类（带快照的 task_progress、不带快照的 task_progress、task_updated、
   task_notification）各记一格，逆序下每类只解最新的一行，更旧的同类行凭廉价的 task_id 截取直接跳过；只有 `task_started` 与
   `async_launched` 行总要解码。测试断言**总**解码次数为 O(task 数)，不再只数快照解码。

**Wire / HTTP / 客户端状态机**

10. **HTTP 支持只取 header 和增量取行**（major；§6.1、§6.2.1、PR-10/11/12）。WS 断开时，兜底逻辑对每个 version 有变化的 task 发 HTTP，
    而端点总是返回全部行，所以每个 running workflow 每 5s（`session_list.js:846` 的断线轮询）都要整份重下一次，最多 2000 行、约 1MB 未压缩。
    客户端还会把折叠中的 workflow 也置 `rowsLoaded=true`，与 §7.5"只在展开时拉行"矛盾。现在新增两个查询参数：`rows=none` 只返回
    header + phases；`since=<version>&epoch=&rows_gen=` 只返回 `rev > since` 的行，epoch 或 rows_gen 不符时服务端改回全量。响应带
    `rows_mode: full|delta|none`。兜底刷新、版本缺口、`rows_omitted` 都按本地是否持有行来选：`!rowsLoaded` → `rows=none`，`rowsLoaded` →
    `since=rowsAt`。只有展开时的 `ensureRows` 会拉全量。
11. **客户端分开记 header 版本与行版本**（§6.1、§7.1、§11.2、PR-11）。v3 的 full 帧总会丢掉行，所以每次重订阅（包括内容没变的短暂 WS 抖动）
    都让展开的 workflow 重拉全部行。fetch 在途时再来一个 full 帧，还会再触发一次 fetch；HTTP 响应落地后无条件 `version = H`，header 与版本可能倒退，
    下一个 delta 又会因 `base > H` 再触发一次重同步。现在 entry 分开记 `version`（header 已到的版本）和 `rowsAt`（行已完整的版本）。full 帧在 epoch 与
    rows_gen 都相同时保留行；`rowsAt` 落后时补一次 `since=rowsAt`。fetch 在途时，full 帧只更新 header，不另发请求。HTTP 响应永不降低 header 或 version。
    delta 的缺口判定改为与 `rowsAt` 比较。`ensureRows` 在 fetch 在途时去重。回到一个 session 时，每个展开的 workflow 恰好发一次请求。
12. **suspended 订阅在进程出现时升级**（major→minor；§6.1、§7.6、PR-12）。session 没有进程时订阅（`wshub_subscribe.go:99-128`）的 tab，
    只有在 session 变成 `running` 时才会重订阅（`session_list.js:979-990` 的 case 3）。而 shim 重接时 parent 往往是 `ready`，所以这个 tab 整个 run
    都没有 workflowPushLoop，只能靠 30s 的计数刷新。评审说"正好是重启场景"不准确：正常重启时 `ReconnectShimsCtx` 在 `srv.Start` 之前同步执行，
    受影响的只是运行中 shim 断联、30s 内重接的那个窗口，以及启动时没接上、之后由 reconcile 接上的 shim。修法选客户端一侧：`sessions_update` 处理器里，
    已有的自动订阅恢复逻辑旁边再加一条——当前订阅是 suspended，且刷新后的快照 `protocol` 非空（只有存在进程时才会填，`managed_query.go:166-170`），
    就重订阅。重接本身会推进 gen（`commitShimReattach` 的 `MarkChanged`），并发出 sessions_update（`settleReconnected` → `notifyChange`，
    `router_shim.go:518`）。这样连事件流在同样窗口里的缺口也一并修好了。服务端一侧的方案（无进程分支也起 loop）需要一种不带 unsub 的注册项，
    或者单独一套 generation，改动更大，所以不选。
13. **404 只表示"条目不存在"**（major；§6.1、§6.2、§11.2、PR-10/11）。v3 把校验之后的所有磁盘失败都折叠成 404，客户端收到 404 又会删除条目，
    结果一个可选的磁盘产物就能删掉一个正在跑的 workflow，之后的 delta 再触发重同步、再 404，反复闪烁。现在 404 只用于 session 不存在、task 不在 board
    里、或 remote node 这几种情况。`/workflow` 读盘失败时返回 200，不带 result / logs，置 `result_unavailable:true`。`/workflow_agent` 缺 transcript
    或 journal 时返回 200，置 `transcript:false`，不带 prompt / result。queued agent 统一为 200 只带 label（v3 的 §6.2.2 与 §11.2 互相矛盾）。
    客户端"404 删除条目"只适用于 `/workflow`。
14. **journal 索引与首行读取不再重复做**（§6.2.2、§8.2、§5.3、PR-10）。v3 的索引在首个请求时同步建立，没有 singleflight，per-IP 突发 20 个
    请求就能同时起 20 遍 64MiB 扫描。索引只按文件尺寸增量续扫、不看 inode，文件被替换或截断后会给出过期的偏移。`ReadFirstLineIDs` 与
    `ReadFirstPrompt` 又在每次请求时各解一遍首行。现在索引按 `(task, dev, ino)` singleflight 建立，inode 变化或文件变小就重建。首行改为一次解码
    同时取出 ID 与 prompt，结果按 agentId 缓存在 board 里（LRU 64）。另外写明 journal 的行上限是 `limits.MaxStreamJSONLine`（16MiB），不是 stream 的 10MiB。

**磁盘与路径安全**

15. **`PathContainedInRoot` 参数顺序与大小写不敏感文件系统**（§8.1、§10、§11.2、PR-4/9）。签名是 `PathContainedInRoot(resolved, root)`
    （`osutil/pathroot.go:19`），v3 却写成 `(projectsRoot, …)`。照抄的话，合法的 run dir 会一律被拒；反过来想，这个检查实际问的是"root 是否在
    run dir 之下"，root 的任何祖先都能通过（来源 1 的结构检查与 basename 正则仍会挡住，所以实际影响比评审说的小，但调用本身是错的）。
    另外 inode 兜底放行的是大小写不同的路径，下游却全按字节前缀判断（agentevents 的 `jsonlPathUnderAllowedRoot`，`handler.go:309-346`）。
    现在写成 `PathContainedInRoot(resolvedCandidate, projectsRoot)`，两侧都经过 EvalSymlinks。新增 `osutil.RelUnderRoot(resolved, root) (rel, ok)`，
    返回匹配祖先以下的那段路径；RunDir 一律**按 projectsRoot 的拼写重拼**成 `projectsRoot + rel`，结构检查也解析 rel。这样下游的字节前缀判断天然成立。
    PR-4 顺带把 agentevents 的最终比较也换成 `PathContainedInRoot`。测试：合法 run dir 被接受；projectsRoot 的祖先被拒；darwin 上 root 与
    transcriptDir 大小写不同时仍能解析出相同拼写的 RunDir。
16. **后续的重新打开也不会被 FIFO 卡住**（major；§8.2、§8.3、§10、§5.9 R3a、PR-9/13）。v3 只保护首次打开。`TranscriptReader` 在没有缓存 fd 时
    （`openOrReuse`）和每次零字节轮询时（`reprobeRotation`，`subagent/transcript.go:71-109,237-262`）都要先 `os.Stat` 再 `os.Open` 重新打开，
    而且是在持有 `r.mu` 的情况下。WS 路径经 `ensureTailer` 按路径建 reader（`agent_tailer_registry.go:214`），根本拿不到 §8.2 校验过的 fd。
    rm + mkfifo 之后，tailer 的下一次轮询就会阻塞在 `os.Open` 里；`Close()` 也要取 `r.mu`，所以 tailer 拆不掉。更糟的是，registry 只有一个
    `pollLoop` goroutine 串行推进所有 tailer（`agent_tailer_registry.go:95-110`），一个被卡住，全部 tailer 都停。现在 reader 接收一个注入的 opener：
    每次重新打开都经 opener（默认 `osutil.OpenRegular(path, 0)`），旋转探测改用 `os.Lstat`；workflow tailer 用 board 提供的、锚定在 root 上的 opener。
    这样既有的 Agent tailer 也一并受到保护。目录打开（R3a 的 `ReadDir`）用新的 `OpenDirIn`。中间目录被换成 symlink 的情况，改为在每次读取时用
    `os.OpenRoot(projectsRoot)` 加上 rel 路径打开（Go ≥ 1.24 的 `os.Root` 会拒绝逃出 root 的路径，中间分量也算），不再信任启动时解析过一次的绝对路径。
17. **主 transcript 尾窗是读窗口，不是文件尺寸上限**（major；§5.10、§10、§11.2、PR-3）。v3 写的是"经 `osutil.OpenRegular`（≤ 64KiB 尾部窗口）"，
    又把它列在 `ErrTooLarge` 的尺寸上限里。照抄的话，所有真实的 session JSONL（MB 级）都会被拒，再按"读不到 → midTurn"处理，`unknown → idle`
    的修复在它的主要场景（长 workflow、ring 已绕回）里就失效了。现在改为：`OpenRegular(path, 0)` 打开，从 `max(0, size−64KiB)` 起 `ReadAt`，
    丢掉第一段不完整的行；窗口里一条完整记录都没有时（末条记录本身超过 64KiB，比如很大的 tool_use input），放大到 1MiB 再试一次，仍没有就按 midTurn。
    测试：5MB 的 JSONL、末尾是 `end_turn` → idle；末条记录 200KiB → 放大窗口后正确裁决。

**前端**

18. **嵌套字段靠 JSDoc 类型接受契约检查**（§6.3、§7.1、PR-11）。check-ws-contract 只扫顶层 `msg.<field>`（`check-ws-contract.mjs:83-97`）。
    嵌套读取只沿两条链跟踪：从 `msg` 出发的成员链，或带 def 短名 JSDoc 类型的参数（`scripts/ws-contract-nested.mjs`）。v3 把 `msg.workflow`
    原样交给叶子模块，`rows_gen`、`agents[].rev`、`prev_agent_ids` 之类的读取于是都不受检查。现在 `workflow_state.js` 的入参用 JSDoc 标注
    wsproto 与 `ext/workflows` REST schema 里的 def 短名（`/** @param {WireView} w */` 等），§6.3 加一行。另外更正先例：`cron_view.js:2020-2021`
    就是 R6 有意允许的"转交给参数名为 msg 的同文件函数"，不是逐字段读取；v3"钻空子"的说法删除。两种形态都过 R6，本 RFC 保留逐字段拼对象，
    因为字段一目了然。
19. **UI 细节与 CSS 对齐**（§7.3、§7.4、PR-12）。（1）`.sa-*` 规则全部限定在 `.rb-agent-row` 下（`split_view.css:91-99`），放到 `.wf-row`
    上不起作用。现在在 `views.css` 里写独立的 `.wf-*` 规则。（2）stopped 与 skipped 同为 `--nz-text-mute`；浅色主题下 queued 的 `--nz-text-dim`
    （`#6e7781`）与之几乎相同（`tokens.css:254,258`）。现在不再声称"颜色双编码"：颜色只区分 running / done / failed，中性的三种状态靠字形加
    `.sr-only` 文本区分。（3）键盘弹出时，32vh 按完整视口计算，会占掉剩余可见区域的大半。现在与 banner 一样，`body.kbd-open #workflow-panel` 整体隐藏
    （`responsive.css:182` 的先例）。
20. **终态播报只针对当前打开的 session**（§7.7、PR-12）。workflow 帧会到达每个已订阅的 key（包括 `cron_live.js:37-49` 自己发起的订阅），
    store 也保留非当前 sid 的 header。不加限制的播报会念出用户没在看的 session 里的 workflow 结束，违反 `a11y_live_regions.test.js` 的规则 (d)。
    现在只在帧的 sid 等于当前选中 sid 时才播报；非当前 sid 的终态也记为"已播报"，所以之后切过去也保持安静。e2e 覆盖这两种情况。
21. **`workflow_view.js` 由 `dashboard.js` import；行数预算按实际算**（§7.1、§7.2、PR-11/12）。v3 没定由谁 import（"dashboard.html 或
    event_stream.js"，而 §7.6 又说 event_stream.js 不碰），PR-11 的文件清单里两者都没有，结果 `wsm.on` handler 永远不会注册。现在 PR-11 在
    `dashboard.js` 加一行副作用 import，handler 从 PR-11 起就生效、store 开始填充；PR-12 把它换成具名 import。dashboard.js 的净增约 +6-7 行
    （import、两行 renderMainShell、registerActions 表项、`onSessionsApplied`、会话切换调用），在 18 行余量之内（上限 2878，现状 2860），
    不是 v3 写的 +2。

**工程化**

22. **ratchet 台账更正**（major；§7.1、§14、PR-10 至 PR-15）。PR-10 重生成 `static/contract.js`，新增的两条 `/api` 路由让 API 表多出两行
    （`contractjs.go:57-66`），`contract.js.lines`（基线 110）和 TOTAL.lines 都会上涨，而 v3 没给 PR-10 安排基线修改和台账行，`js-ratchet --check`
    与必过的 `ratchet-raises` 都会红。现在：特性 issue 在 PR-10 **之前**开好；凡是改动 `static/*.js` 的 PR（包括生成的 contract.js）都要追加台账行。
    另外两处更正：per-file `lines` 只按总和计入台账（`tools/ratchet-raises/metrics.go:244-287`），不是台账键，所以台账行写
    `js-ratchet:TOTAL.lines`，per-file 只改基线（v3 在 PR-11 至 PR-15 都把 per-file lines 写进了台账）；`routes.golden.json` 不是 pin，pin 只有
    `test/e2e/golden/pins.json` 列出的文件，所以删掉 `golden:routes`。
23. **`IsValidWorkflowRunID` 前移到 PR-4**（major；PR-4、PR-8、PR-9）。PR-8 的 R0 要用它来复核 Ref.RunID，测试也写了"非法 RunID 被丢弃"，
    可它原本排在 PR-9 的 path helper 里，而 PR-9 又依赖 PR-8。现在把它和 runID 正则放进 PR-4（PR-4 已经在改 claudefs），PR-8 依赖 PR-4。
24. **Windows vet**（§10、§11.5、PR-3/9/13）。必过的 `build-windows` job 会跑 `go vet ./...`，而 vet 会对测试文件做类型检查（`ci.yml:436-457`）；
    `syscall.Mkfifo` 在 windows 上不存在。现在规定每个 FIFO / O_NOFOLLOW 测试都放在 `*_unix_test.go` 里，并加 `//go:build !windows`
    （仓里已有 3 个先例，如 `gitinfo/open_unix_test.go`），§11.5 加 `GOOS=windows go vet ./...`。
25. **§11.4 按 PR 拆分**（§11.4、PR-12、PR-13）。v3 的 PR-12 测试写的是整个 §11.4，其中 drill-in 用例要等 PR-13 的 `switchTo` 第二参与
    `wf-open-agent`。现在拆成 PR-12 的面板 / store / 无障碍清单（"queued 行不可点"是 §7.3 的标记规则，留在 PR-12）和 PR-13 的 drill-in 清单。
26. **R4 的验收真的覆盖 R4**（PR-8、PR-9）。naozhi 运行中杀掉 shim 时，`shimOutlivedSocket` 返回 `PidAlive(shimPID)` = false
    （`process_end.go:86-96`），procEnded 当场就按 §5.6(6a) 标 interrupted，v3 的 PR-9 验收于是没走到 R4 也能通过。现在把"naozhi 运行中杀 shim
    → 立即 interrupted"作为 PR-8 的 (6a) 验收；PR-9 的 R4 验收改为：naozhi 优雅停止（Detach）→ 杀 shim → 重启 naozhi，R0 恢复的条目没有进程可重接，
    ≤ 90s + 一个 tick 内变为 interrupted。

## v3 变更摘要（vs v2）

评审第 2 轮的确认问题逐条对照代码复核后，合并重复项为以下 34 项。个别评审给的修法不对或可以更简单的，按根因另解，在条目里注明。

**并发与生命周期**

1. **锁序 + 通知异步化**（blocker；§5.8、§11.2、PR-8）。v2 的 wake 在 `b.mu` 内直接
   `r.ss.Update(markChanged)`；可三个 bind 点里，`installFreshSession`（`router_lifecycle.go:665` → :798）与
   rename（`router_rename.go:27` → :117）本身就跑在 `r.ss.Update` 事务里。表锁不可重入
   （`sessiontable/tx.go:31-35` 是普通 `t.mu.Lock()`），bind 当场 wake 出结构变化就会在同一 goroutine 上二次加锁、自死锁；
   readLoop 的 wake（b.mu → 表锁）与事务内的 bind（表锁 → b.mu）又构成 ABBA。现在规定锁序
   **表锁 → b.mu → Tracker.mu，持 b.mu 时绝不取表锁**：wake / bind 在锁内只做计算、置标志，sessions_update
   一律交给 board 的 notifyTimer goroutine 异步发出；`Running` / `LastObservedAt` / `Workflows` /
   `WorkflowAgent` 改为无锁原子读，`evictOldest(tx)` 调用它们不取 b.mu；同一 proc 重复 bind（rename）是 no-op。
   新增测试：在 `r.ss.Update` 内 bind 一个含 running workflow 的 proc。
2. **只有 CLI 确实没了才合成 `interrupted`**（§4.2、§5.6(6a)、§5.8、§5.9 R4、PR-11 验收）。shim EOF / 读错 /
   oversize（`process_readloop.go:244-262`）和 `Detach`（`process.go:386`）只结束 naozhi 这一侧的 Process，CLI 与 workflow
   还在 shim 里跑，reconcile loop 每 30s 会重接（`router_shim.go:113-130`）。v2 会在这段窗口发布假终态：播报"已终止"、
   agent 置 stopped，30s 存盘还可能把它写进 Ref。现在直接复用 `cli.ProcessEnd` 的 `Detached` / `ShimLive`
   （`process_end.go:14-32`，`bookProcessEnd` 已经靠它区分）：两者都为假才标 interrupted；否则保持 running，置
   `Degraded=snapshot_stale` 并冻结 `LastObservedAt`，交给重接后的 Tracker、结果文件 sweeper 或 R4 收敛。
   `SetOnEnd` 只有一个槽，所以 board 从 `bookProcessEnd` 的同一回调得知进程结束，不另设回调。
3. **R4 有了触发点**（§5.9、§5.6(6b)）。由 30s sweeper 执行：条目非终态、board 已无存活进程超过
   `workflowOrphanAfter = 90s`（3 个 reconcile tick）、也没有 taskId 匹配的结果文件 → interrupted。之后若重接的 Tracker 带回同一
   task_id，进程侧覆盖回 running。Ref 恢复（R0）的 running 条目走同一条规则。
4. **状态谓词写清**（§4.2、§5.8）。`Running()` = status ∈ {running, paused}（含 snapshot_stale）；sweeper 的
   "未终态"候选另含 unknown（不钉保活，但会去找结果文件）。Ref 持久化 `last_observed_at`，R0 恢复时用它
   （缺省取恢复时刻），6h 钉住上限对恢复条目也有定义。
5. **终态条目的归属与上限**（§5.3、§5.8、§6.2.1、Q11）。v2 的 retained 只在 bind 时写入：Tracker 按 LRU 淘汰
   （每 Process 5 个）的终态条目会从 Published / Summary / HTTP 消失，且 retained、`last`、结果缓存都没有上限，
   每次 respawn / 重接都会往里堆。现在 wake 把"上次发布有、新 Set 里没有"的 task 移入 retained；board 级上限：
   非终态 ≤ 16、终态 5（live 与 retained 合并按 EndedAt LRU），淘汰时 `last`、结果缓存、journal 索引一并删除；Ref 从这个
   有界集合派生（≤ 21 条）；wake 的代价是 O(live + 有界 retained)。Q11 的"更早的 N 个"链接删除：§6.2 没有列表端点，
   Summary 也不带更早的 task_id，评审建议的新列表端点收益太小。
6. **结果缓存的上界重算**（§6.2.1、§6.2.2、§5.3）。v2 的"≤ ~120KB / workflow"不成立：本机 309-agent 的
   `wf_51427dfc-2fd.json` 光 resultPreview 就有 116,790 字符；而且 CC 自己把 resultPreview 截到 ~400 字符，v2 的 8000 runes
   上限形同虚设，同一 agent 的结果在 running → 终态时会从 journal 的 ~8000 字缩成 ~400 字。现在不缓存 per-agent
   resultPreview：per-agent 结果不论 running 还是终态都从 journal 的 agentId → 偏移索引取；缓存只剩 result（16KB）与
   logs（总计 ≤ 64KiB），≤ ~80KB / workflow，计入 §5.3。
7. **StartedAt 的来源；结果文件只校验 taskId**（§4.2、§5.2）。删掉 `rf.StartTime ≥ w.StartedAt − 5s`：task_started
   与 replay 帧都不带时间戳（`router_shim.go:447-451`），seed 出来的 StartedAt 若取观测时刻就是重启时刻，真正的结果文件
   会被永远拒收；taskId 是每个 attempt 新生成的 9 字符随机 id，已足以区分 resume 前后。StartedAt 的优先级：结果文件
   `startTime` > Ref > live 观测到 task_started 的时刻 > 快照里最早的 agent queuedAt/startedAt；seed 帧不使用观测时刻。
8. **`unknown` 重连判定挪进 SpawnReconnect**（§5.10、PR-3）。v2 在 `SetCwdForLinker` 之后读 JSONL 裁决，可那时 readLoop
   已经启动，违反 #1778 的"startReadLoop 之前武装"（`wrapper.go:625-637`、`adopted_turn.go:72-87`）：晚武装会让先到的 result
   被吞掉、session 卡在 Running；先武装再降级又要在 readLoop 运行中撤销只能 settle 一次的 latch。现在由 session 层传入
   resolver（与 `markTranscript` 同源：`sess.Workspace()` + session id），SpawnReconnect 在 `applyReconnectVerdict`
   之前同步调用。
9. **`readEventBuf` 改为 defer 清理**（§4.1.1、§5.2）。`dispatchProtocolEvent` 报告 kill 时 dispatch 循环会提前
   `return shimDispatchReturn`（`process_readloop.go:356-372`），v2 写在循环之后的清理不执行。
10. **Router 预算**（§5.6(6b)、§11.5、PR-3/8/9）。`router_budget_test.go` 把 Router 方法数钉在 98，现状恰好 98，v2 的
    `r.sweepWorkflows` 会让 `go test ./internal/session` 红；改成带 *Router 参数的包函数又会抬 `routerTypeRefBaseline`。现在
    sweeper、reconnect resolver、R3 对账都写成自由函数 / 闭包，参数里没有 *Router；ProjectsRoot 放进 `HistoryIO`，不加 Router 字段。

**解码与 Tracker**

11. **Partial 解码不得破坏行身份**（§4.1.1、§5.7）。条目本身或身份字段类型错时，encoding/json 会把它们置零：`[1,2]` →
    Type=""、全部当未知类型忽略；`"index":"3"` → 全部撞到 index 0。v2 允许 Partial 帧整体替换行，真实行会被清空，且之后每帧都如此。
    现在身份类错误一律按 Failed（只更新 header，保留旧行）；Partial 帧另做身份校验（Type 非空、agent index ≥ 1 且唯一），
    不过即降为 Failed。另外 encoding/json 只报第一个类型错，workflow_progress 里的错会遮住后面 `status` 等字段的错，
    规则 4 因此依赖键序；现在 Partial / Failed 时再用一个遮蔽了 workflow_progress 的结构体解一遍，把其余字段的错暴露出来。
12. **journal 不再生成 agent 行；新增 `rows_gen`**（§5.2、§5.9 R3c、§6.1）。journal 行没有 `index`（实测
    `wf_3c5f7769-1b7/journal.jsonl` 0 处），R3c 只能造合成 index，首张 live 快照整体替换后，客户端"按 index 合并、行永不删除"会留下幽灵行。
    删掉 R3c 的行生成与 `ApplyJournal`：running 而一张快照都没有的 workflow 只显示 header（`Degraded=no_snapshot`），下一张 live 快照（≤ 10s）补齐。
    另加 per-task `rows_gen`：board 发现某个 index 从行集里消失时递增，推送端对该 task 重发 full，客户端丢掉本地行，"行不会被删除"
    成为每个 rows_gen 内由服务端保证的不变式。
13. **`Agent` 不可比较**（§5.8）。它含 `PrevAgentIDs []string`，`==` 编不过。改为手写 `agentEqualIgnoringRev`（标量字段 +
    `slices.Equal`），并用反射钉住字段数，新增字段忘了比较即红；另测"只有 PrevAgentIDs 变化也推进 rev"。
14. **`background_tasks_changed` 从 IsWorkflowTask 规则 2 删除**（§5.7）。Event 不解它的 `tasks` 数组，SeedFromReplay 的
    前缀门控也不收这个 subtype，这个分支永远不会命中；task_started 与 workflow_progress 已够用，不为它加定向解码。
15. **readLoop 上的规范化与合并代价**（§5.2、§5.3、R2、§11.2）。v2 只给解码做了预算，而 Observe 的 redact + 截断、行重建、board diff
    同样跑在 readLoop 上。实测真实 404 行快照全部 redact 0.21ms；含 `=` 的串走正则，每串约 7.7µs，2000 行可到 ~15ms。现在 builder 按 index
    记原串的 maphash，原串没变就沿用上次的规范化结果（done 行不会再算）；agentId 集合不变时复用 `byAgentID`；新增
    `BenchmarkObserve_Snapshot398` 与 2000 行用例，各有绝对预算；R2 写明全部代价。
16. **漏掉的字符串上限**（§4.3、§5.3）。Name、Description、Current、NotifySummary、RawState、RawStatus 补进"先 redact 再截断"
    表；Current 直接来自 CC 的 `"<phaseTitle>: <label>"`，同样先 redact 原串再截断。

**Wire / HTTP / 磁盘**

17. **新帧 `workflow_set`**（§6.1、§6.3、§7.6、PR-11）。v2 无法告诉客户端"某个 workflow 已经不在了"：`/new` 或驱逐重建后新 board
    往往是空的，一帧都不发，旧 board 的 running 条目就永远 running（sid 不变，store 不清）。现在每个订阅在开始时、换 board 时
    （空 board 也发）、task 集合变化时发 `workflow_set{key, epoch, task_ids}`，客户端删掉不在列表里或 epoch 不同的条目；
    无推送场景下 store 以 Summary 为准。
18. **计数型 sessions_update 改用 `Router.BumpVersion()`**（§5.8、§7.6、R12、PR-12/15）。WS 连着时 `fetchSessions` 在 `stats.version`
    不变时提前返回（`session_list.js:96-103`），sidebar 与 applied hooks 都在其后；`notifyChange` 不推进 gen，所以 v2 的计数更新会被前端丢掉；
    用 markChanged 又会每 30s 重写一次 sessions.json。`BumpVersion`（`router_core.go:743`）= BumpGen + notifyChange，正是为此设计。
    `onSessionsRefreshed` 挂在 `onSessionsApplied`（版本检查之后执行），R12 写明真实代价。
19. **终态帧也有深度门**（§6.1）。v2 的终态帧绕过深度检查、每 250ms 重试，每次失败都累加按 client 共享的 `c.dropped`
    （`wsclient.go:120-148`），几个 workflow 同时结束就能在几秒内把积压的 client 踢掉。现在终态帧在 `len(c.send) < cap/2` 时才尝试入队，
    超过时只重新 arm timer、不调用 `trySendRaw`，不计 drop。
20. **HTTP 校验与 429**（§6.2.3、§6.1）。两个 handler 先 `session.ValidateSessionKey`（400），再判 `SessionFor == nil`（404）；
    客户端遇 429 保持当前状态、清 `fetchInFlight`、按退避重试（有 `Retry-After` 时取较大者）。
21. **HTTP 响应带 `server_now`**（§6.2.1、§7.4）；还不知道时钟偏移时不显示 elapsed，且 elapsed 下限为 0。
22. **RunDir 只由 board 经一个函数生成**（§4.2、§8.1、§10）。Tracker 在 internal/cli 的叶子包里，拿不到 ProjectsRoot；v2 没说 session 层
    的读盘方在哪里校验，Ref 恢复的 SessionID 也没有复核。新增 `claudefs.ResolveWorkflowRunDir`（ID 正则、EvalSymlinks、
    `PathContainedInRoot`、结构检查），board 是 RunDir 的唯一写者；路径 helper 对非法输入返回 ""。
23. **run 目录下的文件统一用一个打开函数**（§10、§6.2.2、§5.9）。v2 只给结果文件 O_NOFOLLOW + Fstat；journal、meta.json、agent jsonl
    （Lstat 后 `os.Open`，TOCTOU）都没有。而 O_NOFOLLOW 挡不住 FIFO：O_RDONLY 打开 FIFO 会阻塞到有写者出现，加上"每个 board 至多一个在途读取"，
    一个 mkfifo 就能永久卡住该 board 的 sweeper。新增 `osutil.OpenRegular(path, maxBytes)`：O_NONBLOCK|O_NOFOLLOW|O_CLOEXEC 打开、Fstat
    必须是 regular、尺寸上限、经同一 fd 读取；R3a 用 `f.ReadDir(257)`；在途读取超过 30s 视为卡死，允许发起新的。
24. **replay 预算措辞**（§5.9）。v2 的"合计"漏了 `drainReplay` 对每个 envelope 的反转义（每个大行 ~1ms / ~1MB，50MiB ≈ 100ms / 100MB，
    既有行为）；现在写明 ≤ ~20ms / ~5MB 是在它之上新增的。

**前端**

25. **check-enum-literals 收窄**（§6.3、PR-11）。WORKFLOW_STATUS / AGENT_STATE 的值（running、failed、completed、queued……）在现有十几个
    static 文件里作为 session / cron / agent 状态被比较，全仓禁令首跑即红（`SESSION_STATE` 不纳管也是这个原因，
    `check-enum-literals.mjs:14-18`）。现在只查：显示表恰含枚举键 + 字面量禁令只覆盖 `workflow_state.js` / `workflow_view.js`。
26. **`wsm.on` handler 的形态**（§6.3、§7.1）。check-ws-receivers R6 拒绝把 `msg` 转交给 import 进来的函数（`check-ws-receivers.mjs:127-128`），
    v2 的 `applyFrame(store, msg)` 跨模块调用必红；包一层同文件函数虽能绕过，却违背 R6 的本意。现在 handler 里逐字段读 `msg.<field>`
    拼成普通对象再交给叶子模块（先例 `cron_view.js:2020-2021`）；PR-11 加 R6 夹具。
27. **展开检测**（§7.2）。`toggle` 不冒泡、不在 `nz_util.js` 的委托列表里，新模块又不能在顶层挂 document 监听（`nz/no-module-side-effects`）。
    改为 `renderWorkflowPanel` 给自己创建的每个 `<details>` 挂 toggle 监听；自动展开走同一个"需要行"路径；只有用户触发的展开才写 sessionStorage。
28. **行的 DOM 约定**（§7.3、§8.3、§8.4）。workflow 行不用 `.rb-agent-row[data-task]`：`agent_view.js:638-647` 的 document 级监听会再调一次
    不带 crumb 的 `switchTo`。改用 `wf-*` 类 + `data-agent-id`。§8.3 的"客户端已会重试 pending"不实：HTTP 200 之后 WS `pending` 几乎不可达，
    真出现时按 capacity 的方式回退到 3s HTTP poll。
29. **`.sr-only`**（§7.7）。仓里没有 `nz-sr-only`，视觉隐藏类是 `.sr-only`（`css/split_view.css:148`）。
30. **跳过 remote node**（§6.1、§7.6、PR-15）。多节点模式下远端 session 原样带着 `workflows` 合进 `/api/sessions`（`list.go:266-296`），
    兜底刷新会对它们反复请求注定 404 的 HTTP，徽标也会显示看不了的 workflow；node 非 local 一律跳过。
31. **前端 store 不再无限持有行**（§7.1、§7.6）。切走 session 时丢掉 rows、置 `rowsLoaded=false`，回来时经 gzip HTTP 重拉。

**工程化**

32. **PR-4 复用 `HubOptions.AllowedRoot`**（§8.1、PR-4）。它只被 `newTailerRegistry` 读（`wshub.go:156`），直接把解析后的 projects root 放进去，
    不加字段（`hubOptionsFieldBaseline = 16`）。§8.1 更正 root 的来源：`agentevents.New` 今天自己调 `os.UserHomeDir()`；PR-4 补上
    agentevents `Deps` 与 `wshub_wired_linkers_test.go`。
33. **PR-10 的 `prompt` 依赖 PR-13 才有的 helper**（§6.2.2、PR-10、PR-13）。`ReadFirstLineIDs` 与新的有界 `ReadFirstPrompt` 前移到 PR-10。
34. **大快照生成器改为包内测试 helper**（§11.1）。`//go:build ignore` 的文件在 `go test` 时不会运行；改为 `_test.go` 里的 `bigSnapshot(n int) []byte`。

## v2 变更摘要（vs v1）

评审第 1 轮的确认问题（不少是同一缺陷被多位评审分别报告）按主题归并为以下 37 项，每项注明改了哪里、为什么。

**解码与 CC 语义**

1. **解码代价重测，删掉"比今天跳过还便宜"**（§1.2.3、§5.1、§11.2、R2）。v1 的数字是噪声
   run；`-count 5` 重测：跳过 0.61-0.63ms / ~0.5KB，typed slim 0.83-0.88ms / 286KB，v1 设计的
   自定义 `UnmarshalJSON` 包装 1.40-1.44ms（多扫一遍值）。改为**不要包装类型**：
   `WorkflowProgress []WorkflowItem` 直接声明在 Event 上，容错改由 `ReadEventInto` 识别
   `*json.UnmarshalTypeError` 且 `Field` 以 `workflow_progress` 开头来实现（encoding/json
   遇类型错会继续解完其余字段，已实测）。同时去掉了 v1 里不能编译的
   `(*[]WorkflowItem)(w)` 转换。bench 闸门从"≤ 跳过 1.1×"（分配量差 3 个数量级，不可能
   达到）改为绝对预算 ≤ 1.5ms、≤ 400KB / 398-agent 快照、`-count≥5`。
2. **`"workflow_progress":null` 等同缺失**（§4.1.1、§5.7）。新解码下 `null` → nil、`[]` →
   非 nil 空切片，`Present = Items != nil`；v1 包装会把 `null` 当成"空快照"清空全部行。
3. **agent 状态规范化顺序改为先判终态**（§4.2）。CC 对安全分类器拦截、排队路径 catch、
   限流撤销都会发**无 agentId / 无 startedAt 的 `state:"error"`** 项，v1 的首条规则把它们判成
   queued。新顺序：done → error（`skipped` / `"skipped by user"` → `skipped`，其余 → failed）→
   无 startedAt 且无 agentId → queued → start/progress → running。新增 `skipped` 状态。
4. **agentId 粘滞 + attempt 历史**（§4.2、§5.2、§8.2-8.4、Q6）。CC 对每项整体替换
   （`Mbr` 的 `n[d]=l`），限流重排队时发出的项不带 agentId，v1 下运行中的 agent 会丢失
   agentId，drill-in 与 tailer doneFn 一起失效；重试每个 attempt 换新 agentId。builder 按
   index 保留最后一个非空 agentId 和 ≤8 个历史 agentId，`byAgentID` 为历史 id 也建索引。
5. **L3 不再凭 `summary` 建条目**（§5.7、§5.10、§9）。非 workflow 的 `task_progress`（mcp_task
   后台状态无条件带、local_agent 开启 progress summary 时带）也有 `summary`，v1 会造出幻影
   running workflow，进而钉住保活最长 6h。只认 `workflow_progress` 键存在，或已知该 task 是
   workflow（`task_type`、`WorkflowLaunch`、Ref）。L3、§9 与 PR-2/PR-7 的 InjectHistory 过滤
   共用一个判定函数。
6. **workflow 状态词表对齐 CC**（§1.2.2、§4.2）。`task_notification.status` 的词表是
   `{completed, failed, stopped}`，`stopped` 映射为 `killed`（v1 会落成 unknown）。
   `adopted` 从不上 wire，从 Status 里删掉。被 kill 的 run **也会写** `wf_<runId>.json`
   （`status:"killed"`），只有 CLI 进程死亡才没有文件。
7. **resume 语义更正 + 结果文件按 taskId 校验**（§1.2.2、§5.5、§5.6、§5.9、§6.2.1、Q9）。
   新 task_id 来自 `resumeFromRunId`（同 runId、新 taskId、同一 journal、不写 `launched`），
   不是 `paused` 占位（那是 adopt 路径，taskId 不变）。续跑期间旧 attempt 的
   `wf_<runId>.json` 仍在盘上；v1 的 R3b、§5.6(5b) 兜底、HTTP 只看 runId 和文件存在与否，会把
   正在跑的续跑判成终态。现在所有读结果文件的地方都要求 `file.taskId == Workflow.TaskID`，
   不匹配即视为不存在。
8. **hook / control_response 快路径改为行首锚定**（§5.1(3)、PR-1、R9）。v1 说
   `"subtype":"hook_` "只能出现在结构位置"，这不对：模型写的 tool_use `input` 对象本身就是
   结构化 JSON。改为 `strings.HasPrefix(line, `{"type":"system","subtype":"hook_`)` /
   `{"type":"control_response"`；结构性兜底里的 control_response 分支改为也走
   `parseControlAck`，以免 CC 改键序时丢 ack。
9. **Workflow tool_use 的 Detail 已在泄漏脚本**（NG2、§10、PR-5）。今天 `FormatToolInput`
   对 Workflow 走默认分支，输出 `Workflow: <原始 input JSON 前 300 字节>`，脚本就在里面，已
   进 ring、持久化、上 wire（本机 event log 实测）。PR-5 新增 `case "Workflow"`，从不输出
   `script`/`args`。历史日志保留原样（NG7）。

**进程侧 / session 侧并发与内存**

10. **board 回调不再乱序，也不受旧进程污染**（§5.2、§5.8、§5.9 R2）。v1 在锁外用传入的 Set
    回调，三个写者之间可能以旧覆盖新。现在回调只是唤醒信号：board 在自己的 mutex 内
    `Load` Tracker 的当前 Set，只接受 `Set.Version > lastApplied` 且来自当前绑定进程的 Set；
    structural 由 board 自己对比 Summary 得出。绑定挪到 `commitShimReattach` 成功之后（与
    `bookProcessEnd` 同位），被放弃的 reattach 不会往 board 里发布。
11. **MergeDisk 来源优先级与应用前置条件**（§5.2、§5.6、§5.9）。stream 快照 > journal；
    结果文件（taskId 匹配）是终态的权威来源。journal 行只在加锁复核"仍 running 且至今没有
    stream 快照"后才应用；读盘前捕获前置条件，应用时复核。
12. **`readEventBuf` 滞留快照**（§5.2、§5.3）。readLoop 解进 `p.readEventBuf[0]`，再把值拷贝
    交给 dispatch；只清拷贝会让每个 Process 钉住最后一张快照（~286KB）。dispatch 循环之后也
    清 `readEventBuf`。
13. **保活改动的其他调用点**（§5.8）。`turnOutstanding` 不再 OR 进 workflow（`scratch.go:225`
    也调它，scratch 会因此永不老化）；改为新谓词 `workflowPinned(now)`，带 `workflowPinMax`
    上限，用在 ReleaseIdleProcess / Cleanup 上；scratch 改为从最后一次 workflow 观测起算老化。
    `evictOldest` 先跳过、无候选时再回退，所以 `takeoverHasSlot` 的"可驱逐"判定仍然成立。
14. **结果文件读取有了触发点，兜底 tick 改到 30s**（§5.6(4)(6)、PR-9）。v1 把兜底挂在
    `Router.Cleanup` 上，可它每 TTL/2（默认 15 分钟）才跑一次，"60s"无从谈起；v1 也没写
    正常终态之后什么时候读结果文件。现在：进入终态时异步读一次（taskId 校验）；另在既有
    30s `saveTicker` 分支上挂一个只做内存判定的 sweeper，负责 stat 与重试。PR-9 验收改为
    "≤ 90s"。
15. **先 redact 再截断，且只做一次**（§4.3、§5.3、§6.1、§10）。v1 先截断、上 wire 时再
    redact，截断后的密钥残段可能短于 `RedactSecrets` 的 minTail 而原样泄出；而且每个订阅者
    每帧都要跑一遍正则。现在 Tracker 规范化时对原串 redact 后再截断，WireView / Summary /
    Ref 都从已脱敏字段派生。

**Wire 协议**

16. **版本由 board 统一分配，行 rev 按内容重盖**（§4.2、§5.8、§6.1）。v1 的 Version 是各
    Tracker 自己的计数器，Ref → 进程、respawn、shim 重连换 Tracker 后会回退，delta 会静默漏行。
    现在 board 为每个 task 维护单调递增的 wire version，每次发布逐行对比内容，变了的行盖新
    rev。另加 `epoch`（board 创建时随机），跨 naozhi 重启的两套版本空间不会互相比较。
17. **客户端 delta 规则改为区间判定**（§6.1、§7.5）。v1 要求 `base_version === local.version`，
    只要混入一次 HTTP 全量就会无限重拉。新规则：`version ≤ local` 忽略；
    `base ≤ local < version` 应用；`base > local` 才 HTTP 重同步。HTTP 拉取在途期间缓存收到的
    delta，落地后重放。
18. **初始帧只带 header + phases**（§6.1、§7.5）。v1 订阅时为每个 workflow 推带全部行的未压缩
    full 帧，而首屏又走 HTTP，行被传了两遍，终态 / 折叠的 workflow 的行也白传。现在 full 帧
    永不带 agents，展开的 workflow 才走 gzip HTTP 拉行。
19. **尺寸硬上限覆盖每一帧**（§6.1、§7.5）。v1 只限 full 帧，且"只带非 done 行"并不能真正
    限住尺寸。现在每帧序列化后 ≤ 192KiB：按 failed → running → queued → stopped/skipped → done
    的优先级装行，装不下的计入 `rows_omitted`。
20. **拆开 `agents_truncated` 的两种含义**（§4.2、§6.1、§6.2.1）。`agents_capped` 表示 Tracker
    的 2000 行上限，HTTP 也拿不到更多，永不重拉；`rows_omitted` 是 wire 专用，HTTP 补拉一次。
21. **背压按队列深度判定**（§6.1、R5）。v1 是 `len(c.send) > 192`，而 workflow 帧可能很大，
    一个 client 可以排队约 48MiB。现在 `len(c.send) > 8` 时跳过非终态 workflow 帧，配合 latest-wins。
22. **推送用独立的 `workflowPushLoop`**（§6.1）。v1 只给 eventPushLoop 加一个 select arm，遗漏了：
    eventPushLoop 卡在 `resubscribeEvents` 时不再推送（恰好是进程退出 → interrupted 那一刻）、
    session 替换时不换 board、suspended 分支没有推送、订阅 board 与快照之间有竞态、
    `lastSent` 无法交接。新的 goroutine 按订阅 generation 管理生命周期，先订阅再快照，
    初始帧自己发，每次发送前核对 generation。suspended / 订阅过期的场景由
    `sessions_update` + Summary 版本触发 HTTP 兜底。
23. **sidebar 徽标不再冻结**（§5.8、PR-15、Q12）。v1 只在结构变化时发 sessions_update，计数
    整个 run 期间都不动。现在每 session 的计数变化至多每 30s 发一次（trailing edge），并写明
    代价。

**HTTP / drill-in / 磁盘**

24. **drill-in 的 board 查找挪到 linker nil 判定之前**（§8.2、§8.3）。v1 放在
    `QueryOrResolveFast` 前，但两个 handler 更早就会因 `linker == nil` 返回 404 /
    `no_linker`，R4 恢复与尚未 spawn 的 session 都无法 drill-in。
25. **transcript 尚未落盘时返回 202，不是 404**（§8.2）。agentId 先于 `agent-<id>.jsonl` 出现，
    404 会让 `agent_view.js` 弹 toast 并跳回 parent。
26. **首行身份校验支持 > 32KiB 首行**（§8.2）。`readFirstLineMeta` 不导出，且首行超过 32KiB
    即拒（本机 1807 个 workflow agent 中有 8 个，最大 355KB；`sessionId` 位于巨大的 `message`
    之后）。新增导出 helper，用有界 `json.Decoder` 只解两个键。
27. **HTTP 限流说法不实**（§10、§6.2）。`sendLimiter` 只挂在 send/bind/upload 上。新端点注入
    per-IP limiter（仿 `ext/memory`），终态结果文件解析一次后缓存，journal 查找建 key→offset
    索引。
28. **claudeDir 只有一个来源**（§8.1、Q8）。`r.hist.claudeDir` 并非可配置，就是
    `$HOME/.claude`，Q8 不成立。所有 workflow 路径与 PR-4 的 tailer 共用 `agentevents.New`
    的那个 EvalSymlinks 后的 ProjectsRoot。
29. **InjectHistory 孤儿 progress**（§5.10、PR-2、PR-7）。workflow 的 task_start 被挤出 500 条
    窗口后，同批构建的 id 集合抓不到孤儿 progress。PR-7 起在 workflow 的 progress / updated /
    notification 条目上打 `TaskType=local_workflow`；遗留日志用 CC 的 id 形态
    `^w[0-9a-z]{8}$` 作启发式（只在这里用）。
30. **重连判定：walk 走完只剩中性帧时**（§5.10、PR-3）。若 ring 已绕回，返回 `unknown`，由
    session 层读 JSONL 尾判定，而不是 idle（会把正在跑的前台长工具误判为 Ready），也不是
    无条件 midTurn（会让修复失效）。
31. **replay 的另外两遍解码**（§5.9、§5.10）。linker walk 与 reconnectVerdict 也会对每行做完整
    解码，PR-5 之后还包括 typed 快照。两处都加行首前缀门控，§5.9 给出合并预算。

**前端与工程化**

32. **面板整体限高，只自动展开最新的 running workflow；elapsed 用服务端时钟**（§7.2、§7.4）。
33. **无障碍**（新 §7.7）：面板不是 live region，elapsed `aria-hidden`；终态经 `#sr-announce`
    播报一次；状态字形带文本标签；`<progress>` 有可访问名；可点击行用真 `<button>`
    （`dashActivate` 只认 Enter）。
34. **Token 冲突与已删闸门**（§7.3、§11.5）。`static_style_ratchet_test.go` /
    `static_light_theme_parity_test.go` 已在 04934631 删除，token 纪律靠 review。running 与
    stopped 原先同为 amber，现在 stopped 改用 `--nz-text-mute` + 字形；queued 改用
    `--nz-text-dim`（`--nz-text-faint` 浅色主题对比度约 2:1，只用于装饰）。
35. **注册清单更正**（§6.3、§7.1）：
    - 删掉 `js-deps-freeze` LOAD_ORDER / baseline（只覆盖 7 个旧文件），改引
      `TestStaticJS_ModuleInventory` / `TestDashboardPage_ImportMapAndPreload`。
    - 枚举防漂移改为扩展 `check-enum-literals.mjs`（`TestContractJS_Current` 只查新鲜度）。
    - PR-8 补 `rest.schema.json` 重生成；Summary / Ref 写明 snake_case tag。
    - PR-10 在 `ext/workflows` 自带 schema test + golden，并登记进 `REST_SCHEMAS` 与
      `check-mock-rest.test.mjs` 的 schema 列表；不扩 `ROUTES`（那是 EventEntry 专用）。
36. **ratchet 台账逐 PR**（§14、§11.5）。`ratchet-raises` 是必过 job，只认本 PR 追加的台账行。
    PR-11 至 PR-15 每个都要列出预期的 gate 并追加台账行；§11.5 加
    `go run ./tools/ratchet-raises -base origin/master`。
37. **测试设计**（§11）：
    - 50MiB replay 用解码计数 / 分配断言，不用挂钟。
    - `-race -count=20` 扩到 session / server。
    - 前端 store 拆成叶子模块 `workflow_state.js`，配 `node --test`，在 PR-11 里落地，不等
      PR-12 的 e2e。

## 1. 背景

### 1.1 现状：dashboard 只看到一个叫 "Workflow" 的工具

Agent 调用 `Workflow` 工具后，CC 在**后台**跑一段 JS 脚本，脚本里用 `agent()` /
`parallel()` 派出几十到几百个子 agent，按 `phase()` 分组。工具本身立即返回
"Workflow launched in background"，parent turn 随后结束，之后 workflow 的全部
生命周期都发生在 parent **空闲**期间。

dashboard 对这件事今天几乎看不到：

- `running_banner.js` 只在 `agent` 事件（Agent 工具的 tool_use）上建 agent 行
  （`running_banner.js:248-261`），`task_start/task_progress/task_done` 只能 patch
  已有行（:262-292）。Workflow 是普通 `tool_use`，其 entry 不带 `ToolUseID`
  （`process_event_format.go:123-155` 只给 Agent 设），所以 workflow 的 task 事件
  挂不到任何行上；`toolVerbs` 里也没有 `Workflow`（`running_banner.js:111`），banner
  显示 "使用 Workflow..."。
- banner 在 session 非 running 且无活跃 agent 行时自动隐藏
  （`running_banner.js:207-212`），而 workflow 恰好在 parent 空闲时最活跃。
- `result` / `user` 事件会清空 `turnState.agents`（`running_banner.js:306-326`），
  后端 `applyEntryStateLocked` 同样在 result/user 时清空 `turnAgents/bgAgents`
  （`eventlog_agents.go:299-341`）——任何挂在 turn 状态上的 workflow 视图都会在
  launching turn 的 result 到达时立即被抹掉。
- Workflow 的 tool_use Detail 走 `FormatToolInput` 默认分支
  （`clievent/tool_input.go`：`return toolName + ": " + TruncateRunesBytes(input, 300)`），
  即原始 input JSON（含 `script`）的前 300 字节；本机
  `~/.naozhi/events/08856cb4….log:46` 可见 `"detail":"Workflow: {\"script\":\"export const meta = {…`。

### 1.2 实测证据

#### 1.2.1 Stream 帧序列（3-agent 探针）

探针：`claude -p` 跑一段固定脚本（phase `Ask` 并行两个 agent A/B，phase `Sum` 一个
agent C，中间 `log()` 一行）。原始捕获：`/tmp/nz-wfprobe/stream-sample.jsonl`
（20 行）。逐行：

| 行 | 帧 | 要点 |
|---|---|---|
| 2 | `assistant` tool_use `name:"Workflow"`, `input.script` | 工具调用 id `toolu_bdrk_011p…`；input 键只有 `script` |
| 3 | `system/background_tasks_changed` | `tasks:[{task_id:"w113pvmto",task_type:"local_workflow"}]` |
| 4 | `system/task_started` | `task_id`、`tool_use_id`、`description:"tiny probe"`、`task_type:"local_workflow"`、`workflow_name:"probe"`、`prompt:<整段脚本>` |
| 5 | `user` tool_result | content 文本含 `Task ID` / `Transcript dir:` / `Script file:` / `Run ID: wf_2997921d-435`；**顶层 `tool_use_result`** = `{status:"async_launched", taskId, taskType:"local_workflow", workflowName, runId:"wf_2997921d-435", summary, transcriptDir, scriptPath}` |
| 6 | `system/task_progress` + `workflow_progress`(4 项) | `description:"Ask: A"`、`last_tool_name:"A"`、`summary:"tiny probe"`、`usage{total_tokens,tool_uses,duration_ms}`；**agent B `state:"start"` 但没有 `agentId`/`startedAt`，只有 `queuedAt`**——"排队"不能靠 `state` 判断 |
| 7-8 | `task_progress` + 快照 | A done（tokens 17739、`resultPreview:"4"`） |
| 9-10 | `assistant` | parent 回复 "Workflow 已经启动（Run ID: …）" |
| 11, 13 | `task_progress` + 快照(5 项) | phase `Sum` / agent C |
| 12 | `task_progress` **无** `workflow_progress` | 只有 description/usage |
| 14 | `background_tasks_changed` `tasks:[]` | |
| 15 | `system/task_updated` | `patch:{status:"completed", end_time:1791170031523}`，**无 description/summary** |
| 16 | `system/task_notification` | `status:"completed"`、`output_file:/private/tmp/claude-503/…/tasks/w113pvmto.output`、`summary:"Dynamic workflow \"tiny probe\" completed"`、`usage`，**无 description** |
| 17-20 | `init` → `assistant` → `result` ×2 | 末行 `result.origin.kind="task-notification"`：CC 自己起的 turn |

注意：探针用的是一次性 `-p`，launching turn 的 result（行 19）被推迟到 workflow
结束后才出；naozhi 的长驻 stream-json 模式下 result 先出、progress 在空闲期到达
（§1.2.4）。设计**不得依赖** result 与 workflow 帧的相对顺序。

#### 1.2.2 CC 源码行为（2.1.288 二进制内嵌 JS，按字节偏移检索）

- **快照是全量，不是 delta。** SDK 发射点：
  `let Gt=nn?F.workflowProgress.filter(er):void 0; Une({taskId:r, …, description: vt?(vt.phaseTitle?`${vt.phaseTitle}: ${vt.label}`:vt.label):n.description, …})`；
  `task_progress` 构造为 `{subtype:"task_progress", task_id, tool_use_id, description,
  usage:{total_tokens,tool_uses,duration_ms}, last_tool_name, summary, workflow_progress}`。
  `workflow_log` 项被 filter 掉，所以 `log()` 行**实时不可见**，只在完成时写进
  `output_file` 与 `workflows/wf_*.json` 的 `logs`。
- **`Une` 是唯一的 `task_progress` 发射器，非 workflow 也调用它并带 `summary`**：
  local_agent 的 progress summary 更新（`Une({taskId:e,…,subagentType:M,…,summary:n})`，
  受 `agentProgressSummaries` 选项门控，naozhi 目前未开）与两条 MCP 后台任务路径
  （`reportBackgroundProgress` 的 `summary:M`、mcp_task 状态轮询的 `summary:o`，无门控）。
  所以 **`summary` 不能用来判定 workflow**；只有 workflow 帧带 `workflow_progress`，
  agent 帧带 `subagent_type`。
- **task id 形态**：前缀表 `local_bash:"b", local_agent:"a", …, local_workflow:"w"`，校验函数
  `P6o` 要求 `t.length===9`（探针 `w113pvmto`）。可作弱启发式，不作唯一判据。
- **项的身份是 `${type}:${index}`，且整项替换。** 合并函数 `Mbr` 以
  `a.set(`${g.type}:${g.index}`, l)` 建索引，`n[d]=l` **整项替换**（不做字段合并）：同一 agent
  的重试（`attempt` 递增）沿用同一 `index`，但**每个 attempt 换新 `agentId`**；`agentId` 未启动前
  缺失；后续某张快照里的项若不带 `agentId`，上一张里的 agentId 就此消失。naozhi 必须以
  `index`（agent 全局唯一）为主键，不能以 `agentId` 或 phase title 为主键。
- **agent 项的 `state` 词表是 `start / progress / done / error`**，另有几类特殊项：
  - 安全分类器拦截：`{type:"workflow_agent",…,state:"error",blocked:!0,error,queuedAt}`，
    **无 agentId/startedAt**；
  - 排队路径 catch：`state:"error",error,queuedAt`，**无 agentId/startedAt**；
  - 限流等待：构造器 `ue(le,{state:"start"})` 发出 `{…,queuedAt:Date.now(),tokens,toolCalls}`，
    **无 agentId/startedAt**（整项替换后原 agentId 消失）；撤销时 `ue(le,{state:"error",…})`；
  - 用户跳过：`At("error",{error:"skipped by user",skipped:!0})`（带 agentId/startedAt）；
    CC 自己的遥测把它与错误分开计数。
- **发射节奏。** `var gs=16, ws=250, hs=1e4`：内部 batch ~250ms；
  `kn=$e.every(gt=>gt.type==="workflow_agent"&&gt.state==="progress")`，
  `nn=!kn||Dt-Z>=hs`——batch 里只要有非纯 progress 的变化（agent start/done、phase
  变化）就立即附快照，纯 progress batch 至多每 10s 附一次；其余 `task_progress` 只带
  description/usage。
- **仅在 running 时发射。** `if(F?.type!=="local_workflow"||F.status!=="running")return`
  ——workflow 一旦终止就不再有快照；失败 / 被 kill 时最后一张快照里可能仍有 agent
  处于 running。
- **终态与词表**：registry / `task_updated.patch.status` 的取值为
  `pending/running/completed/failed/killed/paused`，终态由
  `bt=…aborted?"killed":ce.error?"failed":"completed"` 决定；**`task_notification.status` 的词表
  是 `{completed, failed, stopped}`**，CC 发送前做 `status==="killed"?"stopped":…`。`"adopted"`
  只是 `Li(signal.reason)==="background"` 分支的内部 onSettled 结果，**从不上 wire、不写结果文件**。
  `"paused"` 是 adopt / background-fork 的检查点占位任务（taskId 不变）。
- **结果文件写入**：`W_o(s,{taskId:r,…,status:bt,startTime:n.startTime,…})` 在
  `if(n.abortController?.signal.aborted)return` **之前**调用，所以 completed / failed / **killed**
  都会写 `wf_<runId>.json`；只有被 background adopt（提前 return）或 CLI 进程死亡时没有文件。
- **resume**：`resumeFromRunId` → `z=r.resumeFromRunId??wf_…`、`Z=qw("local_workflow")`，即
  **同 runId、新 taskId**；`Mxt({…,isResume:true})` 复用同一 run dir 与 `journal.jsonl`，
  `if(!ye) await Et.append({type:"launched"})` 在 resume 时**不写** `launched`；移除同 runId 的旧
  registry 任务后注册新 running 任务。resume 校验（`Ir`）只拒绝仍在跑的任务，**不删除**旧的
  结果文件——续跑运行期间，旧 attempt 的 `wf_<runId>.json`（旧 taskId、终态）仍在盘上。
  CC 的失败 / kill 通知文案会提示用户用 `resumeFromRunId` 续跑，这是常规路径。
- **`withholdScriptFromSdkEvents`** 只在 server-authored workflow 路径置位（会把
  phase 改名为 `phase N`、去掉 `promptPreview`，见 `Qo(Pt)` 分支）。naozhi 历史事件里
  phase 标题是真名（如 `Triage: triage:#3080`），即 naozhi 路径下关闭，但解析必须容忍。

#### 1.2.3 体积与解码代价

本机 17 个历史 workflow：全量快照最大 ~516KB（398 agents），82 agents 为 108KB；去掉
`promptPreview/resultPreview` 后约 1/3（82 agents 37KB）。naozhi 单行上限 10MiB
（`internal/cli/process.go:41` `maxScannerBufBytes`，按转义后的 envelope 计；398-agent 实测 496KB 行 → 533KB envelope，约 +7%），
shim 侧扫描器 10MB（`internal/shim/server_cli.go:217-218`，按原始行计）；shim replay ring 上限 10000 行 /
50MiB（`internal/shim/buffer.go:30,34`）。

**行尺寸天花板**（v4 写明；v3 的"不会截断"只对实测过的大小成立）：全量快照约 1.3KB / agent（含 preview，ASCII），CJK 文本
（UTF-8 每字 3B）更多，所以大约 **4k-7k 个 agent** 时单行就会顶到上限，且之后的快照只增不减。两侧的失败方式不同：

- naozhi 侧：envelope 超过 10MiB → `readLoop: oversized shim message, skipping` 后 `continue`（`process_readloop.go:189-195`），
  Process 不结束。本 RFC 在这个分支里窥视 envelope 前缀（§5.7 的 `snapshot_dropped`），否则面板会冻结在最后一张放得下的快照上、看起来却正常。
- shim 侧：原始行超过 10MB → `Scan()` 返回 false，`readStdout` 的循环**永久**退出（`server_cli.go:29-58`），之后没人读 CLI stdout，
  CLI 写满管道即阻塞，session 卡死。这是 shim 的既有行为，与 workflow 无关（任何超大行都会触发），另开 issue（§13 R14），本 RFC 不改。

Tracker 的 2000 行上限（§5.3）帮不上忙：行必须先被读进来才能被裁剪。实测最大的 workflow（398 agents）离天花板有一个数量级。

解码代价（独立 bench `/tmp/nz-wfbench-skeptic`，Go 1.27.0，M4 Pro，398-agent 真实快照：
496KB 行 / 533KB envelope；`-count 5 -benchmem`，取区间）：

| 变体 | 耗时 | 分配 |
|---|---|---|
| shim envelope 反转义 | 0.90-1.07ms | 1.04MB / 3 allocs |
| Event 解码、**跳过** `workflow_progress`（今天的行为） | 0.61-0.63ms | ~0.5KB / 2 allocs |
| **slim typed**：`[]WorkflowItem`，不声明 `promptPreview/resultPreview`，`error` 为 RawMessage（本设计） | 0.83-0.88ms | 286KB / ~861 allocs |
| slim typed 外包自定义 `UnmarshalJSON`（v1 设计） | 1.40-1.44ms | 286KB |
| 全字段 typed（独立复测） | ~1.45ms | 978KB / 2374 allocs |

结论：slim typed 约为跳过路径的 1.35×（多 ~0.25ms/快照），**不是**更便宜；自定义
Unmarshaler 让 decoder 先扫一遍值切出字节、内层 `json.Unmarshal` 再扫一遍，多 ~60%，故
不用。v1 的 "跳过 2.6ms > slim 2.1ms" 来自噪声 run（`-cpu 1` 复测时跳过路径在 0.7-13ms 间
摆动）。按 §1.2.2 的节奏（结构变化立即、纯 progress ≤ 1/10s），每快照 ~1.9ms（envelope +
Event）对 readLoop 可以接受。

#### 1.2.4 空闲期流量

某 session 日志里 48627 条 `task_progress` 中有 48576 条到达于某个 `result` 之后、下一条
user 消息之前，全部进了 event ring。最大的持久化 log（`~/.naozhi/events/08856cb4….log`，
25.9MB）里 57061 条 `task_progress`（18.3MB，~320B/条），对比 5 条 user、31 条 text；
单个 workflow `wxugl33d7` 61k 秒写了 17985 条（~0.3/s，突发）。500 槽 ring
（`eventlog/ring/eventlog.go:20`）约 30 分钟翻一轮。

### 1.3 磁盘布局（实测，探针 session `04a8fc10-…`）

```
~/.claude/projects/<slug>/<sessionID>/
  subagents/workflows/wf_2997921d-435/          # = tool_use_result.transcriptDir
    journal.jsonl                               # 实时追加；resume 时继续追加
    agent-a2093755b9a9ce8c0.jsonl               # 每个 workflow agent 的 transcript
    agent-a2093755b9a9ce8c0.meta.json           # {agentType:"workflow-subagent", description:<label>, workflowPhase, spawnDepth}
  workflows/
    wf_2997921d-435.json                        # 每个 attempt 结束时写（覆盖）
    scripts/probe-wf_2997921d-435.js            # 脚本文件
```

- `journal.jsonl`：`{type:"launched"}` / `{type:"started",key,agentId,label,phase}` /
  `{type:"result",key,agentId,result}`；**无 taskId、无终止记录**；resume 不写 `launched`，
  续跑 agent 以同 key 复用之前的 result。
- `wf_<runId>.json` 键：`runId, timestamp, taskId, scriptPath, script, result, agentCount,
  logs, durationMs, summary, workflowName, status, startTime, phases[{title}], defaultModel,
  workflowProgress, totalTokens, totalToolCalls`（runId 自带 `wf_` 前缀，文件名不是
  `wf_wf_…`）。**`taskId` 是写入它的那个 attempt 的 task id**，是区分 resume 前后 attempt 的唯一
  字段。本机 21 个 run 中 2 个没有该文件（CLI 进程中途死亡；被 kill 的 run 会写
  `status:"killed"`，见 §1.2.2）。实测大小上限 ~576KB，journal 上限 ~1.16MB（`result` 行含完整
  agent 输出）。
- `output_file` 在 `TMPDIR`（`/private/tmp/claude-<uid>/…/tasks/<taskId>.output`），
  不在 `~/.claude` 下，易失，**不作为数据源**。
- workflow 子 agent **不发**自己的 `task_started`；它们的 transcript 与 Agent 工具子代理
  格式兼容（首行 `sessionId` = parent session，另带 `agentId`），`mapJSONLLine`
  （`internal/subagent/transcript.go:310`）可直接解析。**首行是该 agent 的完整 prompt**，
  本机 1807 个文件 p50 9.3KB、8 个超过 32KiB、最大 355,787B；键序为
  `parentUuid, isSidechain, promptId, agentId, type, message, …, sessionId`（`sessionId` 在巨大的
  `message` 之后）。
- `claudefs/usage.go:227-232` 已经在遍历 `subagents/workflows/*/agent-*.jsonl` 计费，
  是仓内唯一知道 `workflows` 目录的地方（硬编码字符串）。

### 1.4 现有链路逐段缺口

| 环节 | 现状 | 缺口 |
|---|---|---|
| 解码 | `ReadEventInto` 反射解到 `clievent.Event`（`protocol_claude.go:463-469`）；Event 只声明 `task_id/tool_use_id/description/task_type/status/last_tool_name/usage`（`clievent/event.go:50-56`） | `workflow_progress`、`summary`、`workflow_name`、`patch`、user 帧顶层 `tool_use_result` 全部被扫描后丢弃 |
| hook 快速跳过 | `strings.Contains(line, `:"hook_`)` 直接丢整行（`protocol_claude.go:451`） | **误伤**：`"label":"hook_tests"` 这类模型生成的值会让该 workflow 之后**所有**快照（累积的）静默丢失；模型写的 tool_use `input` 对象里的键（`{"subtype":"hook_x"}`）同样命中；`:"control_response"`（:454）同类问题——匹配后 `parseControlAck` 失败即 `return nil` |
| tool_use Detail | Workflow 无专门 case，`FormatToolInput` 默认分支输出原始 input 前 300 字节 | 脚本源码进 ring / 持久化 / wire |
| EventEntry 映射 | `task_progress` 与 `task_updated` 同映射为 `KindTaskProgress`（`process_event_format.go:45`），Summary 缺省为 subtype 字面量（:32） | `task_updated` 的 Summary 是字面 `"task_updated"`、`patch.status` 丢失；`task_notification` 的 Summary 是 `"task_notification"`；`user` 帧无 case（:204 返回 nil） |
| linker | `notifyLinker` 只排除 `local_bash`（`process_readloop.go:663-665`）；shim replay walk 同（`router_shim.go:433`）；`InjectHistory` 不看 TaskType（`process_event_query.go:75-82`） | 每个 workflow `task_started` 白跑 12×250ms Resolve 后 tombstone；description 前缀若恰好等于某个已有 agentType（`Plan: …`）还可能**错链**；InjectHistory 里 task_start 已被挤出 500 条窗口的孤儿 progress 同样触发 |
| ring | 每条 task_progress 占一个 ring 槽并持久化；`KindTaskProgress` 是 Activity（`clievent/kinds.go:62`）覆盖 `lastActivitySummary`（`eventlog_append.go:130-132,292`） | 刷掉真实对话；sidebar activity 显示 phase label 或 `task_updated` |
| 重启 | shim replay 只用于 `reconnectVerdict` 与 linker walk，不进 EventLog（`router_shim.go:409-413`）；两者都对每行 `proto.ReadEvent` 完整解码 | 重启后 workflow 状态全丢；且空闲期 replay 末尾是 `system/task_progress`，`isTurnNeutralEventType` 只认 `control_ack`（`wrapper.go:717`）→ 误判 midTurn，session 卡在 Running 直到 CLI 下一个 result |
| 保活 | `ReleaseIdleProcess` 只看 `turnOutstanding`（`managed_release.go:20,36`）；Cleanup 的 idle TTL 看 `LastEventAt`（`router_cleanup.go:278-305`）；scratch 老化（`scratch.go:225`）与驱逐（`router_capacity.go:130,145`）各有 idle 判定 | 都不知道 workflow：一个安静 phase 长于 TTL 就可能连 CLI 带 workflow 一起被关 |
| drill-in | `agent_events` / `agent_subscribe` 只按 task_id 走 linker，且 linker 为 nil 时最先拒绝（`agentevents/handler.go:142-146`、`wshub_agent.go:130-138`）；`rawScanSubagentsDir` 跳过子目录（`subagent/link_scan.go:120`） | workflow agent 无从解析 |

## 2. 目标与非目标

### 2.1 目标

- G1：workflow header——名称、状态、done/failed/running/queued 计数、tokens、tool calls、
  elapsed。
- G2：按 phase 分组，每个 phase 显示进度。
- G3：per-agent 行——label、状态、model、tokens、tool calls、last tool / last tool
  summary、attempt。
- G4：完成后显示最终结果摘要与 `log()` 日志。
- G5：drill-in 到任一已启动 workflow agent 的 transcript（HTTP 初始页 + WS 实时），包括
  重试前的 attempt。
- G6：parent 空闲时照常更新；naozhi 重启（shim 存活）后恢复到最新状态；shim 也没了时
  从磁盘恢复已完成 run 的终态。
- G7：同一 session 多个 workflow 并发。
- G8：顺带修掉本特性暴露的既有缺陷（hook 误跳过、Workflow Detail 泄漏脚本、linker 白跑/
  错链、重启误判 midTurn、ring 被 progress 冲刷、tailer allowedRoot 错位）。

### 2.2 非目标

- NG1：**不控制** workflow（kill / pause / steer / resume）。stream-json 输入没有对应命令，shim
  只支持 write/interrupt/shutdown（`internal/shim/server_client.go` `handleClientCommand`）。
- NG2：不展示脚本源码与 `args`（`task_started.prompt`、`wf_*.json.script`、tool_use `input`）。
  今天的 tool_use Detail 已经泄漏前 300 字节，PR-5 修掉（§10）。
- NG3：不支持 reverse/remote node 上的 workflow 视图（`agent_events` 对 remote 已 404，
  `agentevents/handler.go:121-140`；reverseconn 只转发 5 类帧，`node/reverseconn.go:714-762`）。
- NG4：不在 IM（飞书/Slack…）侧推送 workflow 进度。
- NG5：不追踪嵌套 workflow（workflow agent 再调 Workflow）：子层事件不进 parent 流。
- NG6：不新增 config 开关（理由 §12）。
- NG7：不迁移已持久化的历史 progress 洪水记录，也不改写历史 Detail。

## 3. 方案总览与备选

### 3.1 选定方案：进程侧 Tracker + session 侧 Board + 专用 WS 帧 / HTTP 端点

```
CLI stdout ──readLoop──▶ ReadEventInto（单次解码，slim typed workflow_progress，类型错容错）
                          │
                          ▼
            dispatchProtocolEvent ── p.workflows.Observe(ev)  ← 不进 ring 的大字段在此消费
                          │            │  (Tracker: 进程内, 单写者, 发布 immutable *Set, 私有 Version)
                          │            └─ wake() ──▶ ManagedSession.workflowBoard（mutex）
                          ▼                              │  Load Tracker Set（版本守卫 + 进程身份）
                 logEventAt（瘦 EventEntry）/ deliverEvent   │  + retained 合并 → 分配 wire version / 行 rev
                 （大字段已清空，readEventBuf 亦清）          ▼
                                                     发布 *workflow.Published（epoch + 版本，immutable）
SpawnReconnect ── Tracker.SeedFromReplay(replays) ── startReadLoop     │
磁盘 journal.jsonl / wf_<runId>.json ── 异步读 + 加锁复核前置条件 ──────┤
   ↑ 触发：终态时一次；saveTicker(30s) sweeper；R3/R4                   ├─ SessionSnapshot.Workflows（摘要，含 version）
                                                                        ├─ Hub: workflowPushLoop → workflow_state 帧
                                                                        └─ /api/sessions/workflow{,_agent}
```

核心原则：

- **快照只在 side tracker 里保留"最新一张的瘦身版"**，永远不进 EventEntry / ring /
  persist / eventCh / readEventBuf。
- **对外版本只由 board 分配**：Tracker 的 Version 只是内部去重与排序依据。

### 3.2 备选方案

- **A：把 `workflow_progress` 挂到 EventEntry 上，让前端从事件流重建。** 否决。ring 500 槽
  × 最大 516KB ≈ 250MB/session；持久化记录上限 4MiB（`eventlog/schema/record.go:41`）
  与 64KiB 复用编码缓冲（`eventlog_bridge.go:161`）都不适配；50 条一帧的 history 会到
  几十 MB，WS 未开压缩（`wshub_admission.go:89-95`）。
- **B：只读磁盘（journal.jsonl 轮询），不碰 stream。** 否决作为主路径。journal 无
  tokens/toolCalls/lastTool/model/状态细节，无 taskId；live run 的 taskId↔runId 映射只在
  stream 里。保留为重启降级路径（§5.9）。
- **C：复用 `SubagentInfo` / `turnAgents`。** 否决。turn 边界清空（`eventlog_agents.go:299-341`），
  而 workflow 生命周期几乎全在 turn 之后；`SetOnAgentTaskDone` 是单订阅者且已被 tailer
  registry 占用（`eventlog_agents.go:351-377`、`wshub_agent.go:83`）。
- **D：Tracker 放在 session 层，靠 `onEvent` 回调喂。** 否决。passthrough / 空闲时
  Event 不经过 Send 回调（`process_send.go:204` 只转发 assistant；passthrough fan-out
  走 `passthroughShouldFanOut`，`process_readloop.go:479-492`），`dispatchProtocolEvent`
  （:498）是唯一 owned/unowned 都经过、且还持有原始 Event 的点。
- **E：给 eventPushLoop 加一个 board select arm。** 否决（v1 方案）。eventPushLoop 在进程
  消失时阻塞于 `resubscribeEvents`（最长 60s，期间不服务其他 arm），suspended 分支根本不起
  loop，`lastSent` 也无法从 `completeSubscribe` 交接；而 workflow 恰好在这些时刻有关键变化
  （interrupted、结果文件终态）。改用独立的 `workflowPushLoop`（§6.1）。

## 4. 数据模型

### 4.1 Event 层新增字段（`internal/cli/clievent`）

以下 Go 代码为示意，省略了部分 struct tag。

```go
// event.go，task 字段块（:49-56）之后
WorkflowName  string         `json:"workflow_name,omitempty"`  // task_started
TaskSummary   string         `json:"summary,omitempty"`        // task_progress: workflow description（非 workflow 也可能有！）; task_notification: "Dynamic workflow \"X\" completed"
SubagentType  string         `json:"subagent_type,omitempty"`  // 仅用作"不是 workflow"的反证
Patch         *TaskPatch     `json:"patch,omitempty"`          // task_updated
WorkflowProgress []WorkflowItem `json:"workflow_progress,omitempty"` // 见 4.1.1；nil = 本帧无快照（含 null）
WorkflowDecode   WorkflowDecode `json:"-"`                     // ReadEventInto 填：ok | partial | failed
WorkflowLaunch *WorkflowLaunch `json:"-"`                      // user 帧 tool_use_result（定向二次解码）
WorkflowTask  bool           `json:"-"`                        // Tracker 标记：该 TaskID 是 local_workflow

type TaskPatch struct {
    Status  string `json:"status,omitempty"`
    EndTime int64  `json:"end_time,omitempty"`
}

type WorkflowLaunch struct { // tool_use_result{status:"async_launched",taskType:"local_workflow"}
    TaskID, WorkflowName, RunID, Summary, TranscriptDir string
}
```

`TaskSummary` 的 JSON 键 `summary` 在 Event 现有字段中无冲突；Tracker 只在 `task_*`
subtype 上读它，且**只作名称回落，不作 workflow 判据**（§5.7）。`output_file` 与 `prompt`
（脚本）**不声明**（NG2、§1.3）。

#### 4.1.1 `WorkflowItem`：slim、无包装类型、容错在 `ReadEventInto`

```go
// clievent/workflow.go（新文件）
type WorkflowItem struct {
    Type       string `json:"type"`        // "workflow_phase" | "workflow_agent" | 其他忽略
    Index      int    `json:"index"`
    Title      string `json:"title"`       // phase
    Label      string `json:"label"`       // agent
    PhaseIndex int    `json:"phaseIndex"`
    PhaseTitle string `json:"phaseTitle"`
    AgentID    string `json:"agentId"`
    Model      string `json:"model"`
    State      string `json:"state"`       // start | progress | done | error（其他透传）
    Attempt    int    `json:"attempt"`
    Cached     bool   `json:"cached"`
    Blocked    bool   `json:"blocked"`     // 安全分类器拦截
    Skipped    bool   `json:"skipped"`     // 用户跳过
    QueuedAt, StartedAt, LastProgressAt int64 `json:"..."`
    DurationMs int64  `json:"durationMs"`
    Tokens     int64  `json:"tokens"`
    ToolCalls  int    `json:"toolCalls"`
    LastToolName    string          `json:"lastToolName"`
    LastToolSummary string          `json:"lastToolSummary"`
    Error           json.RawMessage `json:"error"` // 观测到 string，类型不定 → raw，Tracker 再转
    // 刻意不声明：promptPreview / resultPreview / promptFramed（§1.2.3 解码代价、§4.3）
}

type WorkflowDecode uint8 // WorkflowDecodeOK | WorkflowDecodePartial | WorkflowDecodeFailed
```

容错规则（`ReadEventInto` 内，对 `json.Unmarshal` 的返回值）：

1. `err == nil` → `WorkflowDecodeOK`。
2. `errors.As(err, &*json.UnmarshalTypeError)` 且 `ute.Field == "workflow_progress"`（整个值
   不是数组）→ 接受该 Event，`WorkflowProgress = nil`、`WorkflowDecode = Failed`。
3. 同上但 `Field` 以 `workflow_progress.` 开头：
   - 坏的是**条目本身或身份字段**——`Field` 形如 `workflow_progress.N`（整项不是对象，如 `[1,2]`），或以
     `.type` / `.index` / `.phaseIndex` / `.agentId` / `.state` 结尾 → `WorkflowDecode = Failed`（同规则 2：只更新
     header，**保留上一版的行**）。encoding/json 会把这些字段置零：`[1,2]` 得到 Type="" 的项、全部被当作未知类型忽略；
     `"index":"3"` 让各项都撞到 index 0。按 Partial 处理会用这些项整体替换、清空真实行，而且 CC 只要不改回来，之后每帧都如此
     （go1.27.0 实测）；
   - 其他字段（如实测的 `workflow_progress.0.tokens`）→ `WorkflowDecode = Partial`：坏字段置零，其余项完好。
     encoding/json 遇类型错会记录首个错误并**继续**解完（已实测：description、其余项均正确）。
   - **Partial 的身份复核**：encoding/json 只报**第一个**类型错，后面某项的 `index` 类型错会被前面的 `tokens` 错遮住。
     所以 Partial 帧在 Tracker 应用前再做一遍 O(n) 校验：每项 `Type` 非空；`workflow_agent` 的 `Index ≥ 1` 且互不重复
     （CC 的 index 从 1 起连续编号，实测 309 项为 1..309）；`workflow_phase` 的 `Index` 互不重复。不通过即降为 Failed。
     （这项校验对 OK 帧同样执行，代价可忽略。）
4. 其他错误（语法错、类型错不在 workflow_progress 下）→ 维持今天的行为（返回 err）。
5. **被遮住的其他字段错误**：同样因为只报第一个错误，`{"workflow_progress":[{"tokens":"x"}],"status":5}` 只报
   `workflow_progress.0.tokens`，`Status` 被静默置空；键序反过来则报 `status`、按规则 4 返回 err——是否拒收取决于键序。
   所以规则 2、3 命中时，再用 `struct{ clievent.Event; WP json.RawMessage `json:"workflow_progress"` }` 解一遍同一行（外层
   同名字段遮蔽内层，workflow_progress 只作 RawMessage 跳过）：返回 err → 按规则 4 处理（今天的行为）；否则采用这次解出的
   Event 字段，`WorkflowProgress` 取第一次的结果（Partial）或 nil（Failed）。这一遍只发生在出错路径上（正常为零次）。

于是 CC 改了某个非身份字段的类型时只降级（计数 + `Degraded=decode_error`），不连坐整帧；改了身份字段的类型时保留旧行、
只更新 header；无自定义 `UnmarshalJSON`，正常路径不多扫一遍值。**`Present` 的定义是 `WorkflowProgress != nil`**：
键缺失与 `"workflow_progress":null` 都是 nil（不替换 agent 行），`[]` 是非 nil 空切片（合法的
空快照）。

注意：`ReadEventInto` 返回值语义要求每行至多一个 Event，且 pooled Event 需整体复位
（`protocol_claude.go:33` `resetEvent` 整 struct 赋值），新字段自动被清。但返回值是追加进调用方
`buf` 的**值拷贝**——readLoop 传入的是 `p.readEventBuf[:0]`（`process.go:126`、
`process_readloop.go:330`），所以 `readEventBuf[i].WorkflowProgress` 会一直钉住最后一张快照，直到
下一帧覆盖该槽；hook 等被快路径跳过的帧不触碰 buf。§5.2 规定在 `handleShimStdout` 里用 `defer` 清空
（dispatch 循环有提前 return 的路径）。

### 4.2 Tracker 内部 / 发布态（新包 `internal/cli/workflow`，纯逻辑叶子包）

```go
package workflow

type Status string     // running | completed | failed | killed | paused | interrupted | unknown
type AgentState string // queued | running | done | failed | skipped | stopped | unknown

type Agent struct {
    Index      int        `json:"index"`        // 主键（CC 的 workflow_agent:<index>）
    PhaseIndex int        `json:"phase_index,omitempty"`
    Label      string     `json:"label"`
    AgentID    string     `json:"agent_id,omitempty"`  // 当前或最后一个非空 agentId（粘滞）；从未启动为空 → 不可 drill-in
    PrevAgentIDs []string `json:"prev_agent_ids,omitempty"` // 之前 attempt 的 agentId（≤8，旧→新）
    Model      string     `json:"model,omitempty"`
    State      AgentState `json:"state"`
    RawState   string     `json:"raw_state,omitempty"` // 仅当规范化为 unknown 时保留原值
    Blocked    bool       `json:"blocked,omitempty"`
    Attempt    int        `json:"attempt,omitempty"`
    Cached     bool       `json:"cached,omitempty"`
    QueuedAt, StartedAt, LastProgressAt, DurationMS int64 `json:"…,omitempty"`
    Tokens     int64      `json:"tokens,omitempty"`
    ToolCalls  int        `json:"tool_calls,omitempty"`
    LastTool   string     `json:"last_tool,omitempty"`
    LastToolSummary string `json:"last_tool_summary,omitempty"` // 已 redact 后截断
    Error      string     `json:"error,omitempty"`             // 已 redact 后截断
    Rev        uint64     `json:"rev"` // 由 board 盖章：该行内容最后一次变化时的 wire version（§5.8）
}

type Phase struct {
    Index int    `json:"index"`
    Title string `json:"title"`
    Counts       `json:"counts"`
}

type Counts struct { // json 全部 snake_case
    Total   int `json:"total"`
    Queued  int `json:"queued"`
    Running int `json:"running"`
    Done    int `json:"done"`
    Failed  int `json:"failed"`
    Skipped int `json:"skipped"`
    Stopped int `json:"stopped"`
}

type Workflow struct {
    TaskID      string `json:"task_id"`
    RunID       string `json:"run_id,omitempty"`
    Name        string `json:"name,omitempty"`        // workflow_name，回落 summary/description
    Description string `json:"description,omitempty"` // task_progress.summary
    Current     string `json:"current,omitempty"`     // 最新 description "<phase>: <label>"
    Status      Status `json:"status"`
    RawStatus   string `json:"raw_status,omitempty"`
    StartedAt   int64  `json:"started_at,omitempty"` // 来源优先级见下文；0 = 未知（客户端不显示 elapsed）
    EndedAt, LastObservedAt int64 `json:"…,omitempty"` // LastObservedAt 在 snapshot_stale 期间冻结
    RowsGen     uint32 `json:"rows_gen"`             // board 盖章：行集身份代数，某 index 消失时 +1（§5.8、§6.1）
    Tokens      int64  `json:"tokens,omitempty"`
    ToolCalls   int    `json:"tool_calls,omitempty"`
    DurationMS  int64  `json:"duration_ms,omitempty"`
    Counts      Counts `json:"counts"`
    Phases      []Phase `json:"phases"`
    Agents      []Agent `json:"agents"`            // 按 Index 升序；发布后只读
    AgentsCapped bool  `json:"agents_capped,omitempty"` // Tracker 2000 行上限触发：HTTP 也拿不到更多，客户端永不因此重拉
    NotifySummary string `json:"notify_summary,omitempty"` // task_notification.summary
    Source      Source `json:"source"`             // stream | replay | journal | result_file | ref
    Degraded    string `json:"degraded,omitempty"` // "" | no_snapshot | decode_error | snapshot_stale | snapshot_dropped | too_many
    Version     uint64 `json:"version"`            // 发布态 = board 的 wire version（§5.8）；Tracker 内部另有私有计数

    // 以下不上 wire
    TrackerVersion uint64 `json:"-"` // Tracker 私有：任一字段变化 +1，仅用于 board 的排序守卫
    SnapshotSeq    uint64 `json:"-"` // 已应用的 stream/replay 快照张数
    ResultLoaded   bool   `json:"-"` // 已用 taskId 匹配的结果文件覆盖
    LaunchTranscriptDir string `json:"-"` // Tracker 只记 tool_use_result.transcriptDir 原串，不校验、不用于 I/O
    SessionID string `json:"-"` // launch 时的 CC session id（跨 session chain 定位 run dir）；原串
    RunDir    string `json:"-"` // **只由 board** 写入：锁外经 claudefs.ResolveWorkflowRunDir 解析、回到 b.mu 内复核后写（§5.8、§8.1）；
                                 // 按 projectsRoot 的拼写重拼；Tracker 发布态里恒为 ""；解析完成前为 ""
}

type Set struct { // Tracker 发布，immutable，atomic.Pointer
    Workflows []*Workflow
    Version   uint64 // Tracker 私有，单调
    SeedWrapped bool // SeedFromReplay 时 replay ring 已绕回（§5.9 判据）；board 的 R5 计时用
}

type Published struct { // board 发布，immutable；Snapshot / Hub / HTTP 只读它
    Epoch     string               // board 创建时随机 64-bit，16 hex（字符串：JS number 精度不够）
    Workflows []*Workflow          // running 在前，其余按 EndedAt 倒序；Version/Rev 已是 wire 值
    byAgentID map[string]AgentLoc  // drill-in O(1)；含 PrevAgentIDs
}

type Summary struct { // 进 SessionSnapshot（/api/sessions）的极简形
    TaskID       string `json:"task_id"`
    Name         string `json:"name,omitempty"`
    Status       Status `json:"status"`
    Counts       Counts `json:"counts"`
    Tokens       int64  `json:"tokens,omitempty"`
    StartedAt    int64  `json:"started_at,omitempty"`
    EndedAt      int64  `json:"ended_at,omitempty"`
    CurrentPhase string `json:"current_phase,omitempty"`
    Epoch        string `json:"epoch"`
    Version      uint64 `json:"version"` // 与 workflow_state 帧同一版本空间（§6.1 兜底刷新用）
}

type Ref struct { // 持久化进 sessions.json：board 有界集合的 header（≤ 16 非终态 + 5 终态 = 21 条，§5.8；
                  // live 非终态 > 16 的病态情形取 LastObservedAt 最新的 16 个；LastObservedAt 超过 workflowPinMax 的 unknown 不写）
    TaskID    string `json:"task_id"`
    RunID     string `json:"run_id,omitempty"`
    Name      string `json:"name,omitempty"`
    SessionID string `json:"session_id,omitempty"` // 恢复后必须重新过 IsValidSessionID（§8.1）
    Status    Status `json:"status"`
    StartedAt int64  `json:"started_at,omitempty"`
    EndedAt   int64  `json:"ended_at,omitempty"`
    LastObservedAt int64 `json:"last_observed_at,omitempty"` // R0 恢复用；缺省取恢复时刻（§5.9）
    Counts    Counts `json:"counts"`
    Tokens    int64  `json:"tokens,omitempty"`
}
```

**StartedAt 的来源**（高者优先，已有更高来源时不被低来源覆盖）：taskId 匹配的结果文件 `startTime` > Ref.StartedAt
（之前某次 live 观测落盘的值）> readLoop **live** 观测到 `task_started` 的时刻 > 快照中各 agent 最早的
`queuedAt` / `startedAt`（偏晚，仅作近似）> 0。`task_started` 与 replay 帧都不带时间戳（`router_shim.go:447-451`），
所以 `SeedFromReplay` **从不**把观测时刻当 StartedAt——那会是重启时刻。

**状态谓词**（单点定义在 `workflow` 包，board / sweeper / 保活共用）：

- `IsRunning(st)` = st ∈ {running, paused}（含 `Degraded=snapshot_stale` 的 running）——`Running()`、`workflowPinned` 用它。
- `IsUnsettled(st)` = `IsRunning(st)` 或 st == unknown——sweeper 的"未终态"候选（unknown 不钉保活，但仍去找结果文件）。
- 终态 = completed / failed / killed / interrupted。

**agent 状态规范化**（Tracker 内唯一一处，前端只认规范值；**自上而下首个命中**）：

| # | 原始项 | 规范 |
|---|---|---|
| 1 | `state:"done"`（含 `cached:true`） | `done` |
| 2 | `state:"error"` 且（`skipped:true` 或 `error == "skipped by user"`） | `skipped` |
| 3 | `state:"error"` / `"failed"`（含 `blocked:true`、无 agentId 的排队 catch、限流撤销） | `failed`（`Blocked` 透传） |
| 4 | 无 `startedAt` 且无 `agentId`（不论 `state`；§1.2.1 行 6、限流等待） | `queued` |
| 5 | `start` / `progress` | `running` |
| 6 | 其他 | `unknown` + `RawState` |
| 7 | workflow 已终态而 agent 仍 queued/running | `stopped` |

终态（1-3）必须先于 queued（4）判定，否则 CC 对拦截 / 排队失败 / 限流撤销发的无 agentId 的
`error` 项会显示为 queued、终态后又变成 stopped，永远不会显示为 failed。queued（4）必须先于
running（5），否则探针行 6 的 B（`state:"start"`，只有 queuedAt）会被判成 running。

**agentId 粘滞与 attempt 历史**（builder 每个 index 一份）：

- `lastAgentID`：最后一次见到的非空 agentId。快照项不带 agentId 时（限流重排队、整项替换），
  `Agent.AgentID` 沿用它；行状态仍按上表（会是 queued），但 transcript 已存在，可 drill-in。
- 观测到的 agentId 与 `lastAgentID` 不同且两者非空（重试新 attempt）→ 旧 id 追加进
  `PrevAgentIDs`（去重，保留最近 8 个）。
- `Published.byAgentID` 为当前与历史 agentId 都建索引；历史 id 的 `AgentLoc.Current=false`，
  drill-in 的 doneFn 对它恒返回 done（§8.3）。

**workflow status 映射**：

| 来源 | 原值 → 规范 |
|---|---|
| `task_updated.patch.status` | `completed/failed/killed/paused/running` 原样；`pending` → running |
| `task_notification.status` | `completed`、`failed` 原样；**`stopped` → `killed`** |
| `wf_*.json.status` | `completed/failed/killed` 原样 |
| 识别不了 | `unknown` + `RawStatus` |
| naozhi 合成 | 仅 `interrupted`：**CLI 确实已结束**（`ProcessEnd` 的 `Detached` 与 `ShimLive` 皆假，§5.6(6a)），或 R4 判定（无存活进程 ≥ 90s，§5.9），且无 taskId 匹配的结果文件。只是 naozhi 与 shim 的连接断了（CLI 仍在跑）时**不**合成，改标 `Degraded=snapshot_stale`；另有 `unknown` + `RawStatus="unclaimed"`：有存活进程、但当前 Tracker 长期不认领的 retained 条目（R5，§5.9），不是终态 |

### 4.3 保留 / 丢弃字段一览

| 字段 | 处理 | 理由 |
|---|---|---|
| `promptPreview` / `resultPreview` / `promptFramed` | **解码时就不声明** | 占快照 ~2/3 体积与大部分解码时间；按需从磁盘取（§6.2.2） |
| `lastToolSummary` | 保留；**先 `RedactSecrets` 原串、再截 200 runes** | per-agent 行需要；常含 bash 命令 |
| `label` / `title` / `phaseTitle` | 先 redact、再截 120 runes | 模型生成 |
| `lastToolName` | 先 redact、再截 64 runes | workflow 层的 `last_tool_name` 被 CC 设成 agent label（§1.2.1 行 6），同样是模型文本 |
| `model` | 截 64 runes | |
| `error` | raw → string，先 redact、再截 400 runes | 类型不定 |
| `workflow_name` → `Name` | 先 redact、再截 120 runes | 进 `/api/sessions` 每次刷新（Summary.Name）与 sessions.json（Ref.Name） |
| `task_progress.summary` → `Description` | 先 redact、再截 200 runes | 模型生成 |
| `task_progress.description` → `Current` | **先 redact 原串**、再截 200 runes | CC 拼的 `"<phaseTitle>: <label>"`，含未截断的 label；也是 Summary.CurrentPhase 的来源 |
| `task_notification.summary` → `NotifySummary` | 先 redact、再截 200 runes | |
| 未识别的 `state` / `status` → `RawState` / `RawStatus` | 截 32 runes | 透传值，原本无上限 |
| `blocked` / `skipped` | 保留（bool） | 状态规范化需要 |
| `workflow_log` 项 | 忽略（本就不发） | |
| 未知 `type` 项 | 忽略 + 计数 | 前向兼容 |
| `task_started.prompt`（脚本） | 不声明 | NG2，且可达数十 KB |
| `output_file` | 不声明 | TMPDIR 易失（§1.3） |
| `fallbackModel` / `lastAttemptReason` 等真实数据中见到的额外键 | 不声明（json 忽略） | 需要时再加 |

先 redact 再截断的原因：截断会把 `sk-…` 之类的密钥切成短于 `RedactSecrets` minTail
（`textutil/secrets.go:33-40`：`sk-` 40、`ghp_`/`AKIA` 16）的残段，残段原样泄出。
`RedactSecrets` 幂等，下游再调无害，但不再要求每个订阅者每帧重做（§6.1）。

**规范化结果按原串记忆**：每张快照都是全量，若每帧对每行每个字符串重跑 redact + 截断，代价落在 readLoop 上——
真实 404 行快照全部 redact 一遍实测 0.21ms / 1344 allocs（该快照恰好不含 `=`）；含 `=` 的串走
`envAssignmentRe` 正则（`textutil/secrets.go` 的 `redactEnvAssignments`），每串约 7.7µs，`lastToolSummary` 常是
`go test -run=…`、`FOO=bar` 之类的 shell 命令，400 行约 3ms、2000 行约 15ms。所以 builder 对每个 index 的每个字符串字段记
`(maphash(原串), 规范化结果)`，原串哈希未变就直接沿用上次的结果（done 行此后永不再算），每帧只对真正变化的串付费。
哈希碰撞只会让显示沿用旧值，不影响安全（规范化结果本身已脱敏）。

## 5. 后端设计

### 5.1 解码（`internal/cli/protocol_claude.go`）

1. **单次解码**：`WorkflowProgress` 直接声明在 Event 上，随第一次 `json.Unmarshal`
   （`protocol_claude.go:469`）一起解；只有带该键的帧付出 typed 成本（§1.2.3：0.83-0.88ms /
   286KB per 398-agent 快照，约为今天跳过路径的 1.35×）。类型错按 §4.1.1 容错。输入是别名
   切片（`stringToBytesUnsafe`，:43-48），decoder 只读不留存，string 字段由 decoder 复制，安全；
   `Error` 的 RawMessage 由 decoder 复制字节，同样不别名输入。
2. **user 帧 launch 信息**：仿 `code_change_published` 的定向二次解码（:502-509）：
   `ev.Type=="user" && strings.Contains(line, `"async_launched"`)` 时解
   `struct{ TUR *wireLaunch `json:"tool_use_result"` }`，且 `TaskType=="local_workflow"`
   才填 `ev.WorkflowLaunch`。线上键是 snake_case `tool_use_result`（磁盘 JSONL 才是
   `toolUseResult`，`internal/claudefs/line.go:31`）。门控字符串命中但不是 workflow 的
   情况（例如 Read 了本 RFC）只多一次解码，结果被丢弃。**不**为所有 user 帧解
   `tool_use_result`（Read/Bash 输出可达 MB）。
3. **修 hook / control_response 误跳过：行首锚定**。v1 设想的 `"subtype":"hook_` 子串锚点
   不安全：JSON 字符串值里的 `"` 会被转义，但模型写的 tool_use `input` 对象（MCP 参数、
   Workflow `args`）与结构化 `tool_use_result` 是**未转义的真实键值**，`{"subtype":"hook_x"}`
   照样逐字节命中。改为：

   ```go
   if strings.HasPrefix(line, `{"type":"system","subtype":"hook_`) { return nil, false, nil }
   if strings.HasPrefix(line, `{"type":"control_response"`) { /* parseControlAck 快路径 */ }
   ```

   探针与仓内 fixture（`process_test.go:210`、`protocol_claude_skipfastpath_test.go:19-21`、
   `cli_test.go:446`、`adopted_turn_test.go:420`）的 system 帧一律以
   `{"type":"system","subtype":` 开头、control_response 以 `{"type":"control_response"` 开头。
   CC 若改键序，前缀不命中，帧落到 :475 的结构性兜底，只是变慢。兜底里的
   `if ev.Type == "control_response" { return nil }`（:478）改为同样调用 `parseControlAck`
   （或直接用已解出的 ev 构造 ack），否则键序变化会让 `SetModel` 等 ack 的调用阻塞，而不只是变慢。
4. **Workflow tool_use 的 Detail**：`clievent/tool_input.go` `FormatToolInput` 增
   `case "Workflow":`，只解 `scriptPath`（取 basename）——输出 `Workflow <basename>` 或单独
   `Workflow`；**永不**读取 `script` / `args`。pin 测试：input 含 `script` / `args` 时 Detail 中
   不出现其任何子串。

### 5.2 Tracker：归属与锁

- **归属**：每个 `cli.Process` 一个 `*workflow.Tracker`（`newProcess`/`newShimProcess` 构造时
  创建，非 nil）。理由：`local_workflow` 跑在 CLI 进程里、随 CLI 死；且只有 Process 在
  `startReadLoop` 之前就存在，能在 replay 种子与 live 帧之间做到无竞态（§5.9）。
- **接入点**：`dispatchProtocolEvent` 中 `notifyLinker`（`process_readloop.go:600`）之后、
  `logEventAt`（:610）之前：

  ```go
  p.workflows.Observe(&ev, now)        // 消费快照；设置 ev.WorkflowTask
  ev.WorkflowProgress = nil            // 必须在 logEventAt / deliverEvent 之前清空（局部拷贝）
  ```

  以及 `handleShimStdout` 里 `ReadEventInto` 返回之后立即 `defer`（`process_readloop.go:329-372`）：

  ```go
  events, _, err = ri.ReadEventInto(msg.Line, p.readEventBuf[:0])
  decoded := events // 与 buf 共享底层数组；之后 events 可能被 rpcErrorTurnEnd 换成新切片
  defer func() { for i := range decoded { decoded[i].WorkflowProgress = nil } }() // 清 buf 槽里的原件
  ```

  不能写在 dispatch 循环之后：`dispatchProtocolEvent` 在 deliverEvent 撞上 killCh 时返回 true，循环直接
  `return shimDispatchReturn`（:369），readLoop 随即退出、不再有下一帧覆盖该槽，最后一张快照会随 dead `*Process`
  一直被钉住。`defer` 覆盖所有退出路径；清的是 `decoded` 的元素（即 `[2]clievent.Event` 数组 `p.readEventBuf` 里实际被写的槽，
  `process.go:126`），不受 `events` 之后被替换的影响；`readEventBuf` 只有 readLoop 使用，无并发问题。

  这一位置对 owned / unowned / 空闲 / passthrough 一视同仁（passthrough 的 result 分支在
  :560-581 已自行 `logEventAt` 后 return，但 result 帧与 workflow 无关）。`Observe` 同时设置
  `ev.WorkflowTask`（判定见 §5.7 的 `IsWorkflowTask`；供 §5.10 的 InjectHistory 标记与 §9）。
- **eventCh 内存陷阱**：`deliverEvent` 把 Event 值拷进 cap 1024 的 `eventCh`
  （`process.go:288`、`process_readloop.go:727-742`），空闲 / passthrough 下无人消费，最多
  1024 份滞留。不清空 `WorkflowProgress` 就可能每 session 钉住数百 MB。单测断言：出 eventCh
  的 Event 与喂完一帧快照后的 `p.readEventBuf[*]`，`WorkflowProgress == nil`。
- **锁**：`Tracker.mu sync.Mutex` 只保护可变构建态 `map[taskID]*builder`；写者是 readLoop
  （`Observe`，单 goroutine）、`SeedFromReplay`（`startReadLoop` 之前，无并发）、board 在 bind 时下发
  已知 task_id 集合（§5.7 规则 4）与磁盘合并（`ApplyResultFile`，异步 goroutine，§5.6）。每次产生变化后在锁内递增私有
  `Set.Version`、构建新的不可变 `*Set` 并 `atomic.Pointer.Store`，**锁外**调用 `wake()`。读者
  只 `Load()`，无锁。Tracker 不触碰 ring 的 `l.mu`（ring 约束：`eventlog.go:41-45`、
  `eventlog_persist.go:14-29`）。全局锁序见 §5.8（表锁 → b.mu → Tracker.mu）。
- **结果文件合并的前置条件**：读完后 `t.ApplyResultFile(rf)` 在锁内复核 `rf.TaskID == w.TaskID`（resume 防护，§5.5）——
  **只看 taskId**。v2 另加的 `rf.StartTime ≥ w.StartedAt − 5s` 已删除：StartedAt 若来自重启后的观测时刻（见上文 StartedAt 的来源），
  这个条件会把真正的结果文件永远拒之门外；而 taskId 是每个 attempt 新生成的 9 字符随机 id（§1.2.2），已足以挡住续跑期间旧 attempt 的文件。
  满足即作为终态权威覆盖：status、agent 行（经同一张规范化表，按 index 合并、身份校验同 §4.1.1）、totals、StartedAt
  （`startTime`），置 `Source=result_file`、`ResultLoaded=true`。此后到达的 `task_notification` 只补 `NotifySummary`，不再覆盖 totals。
  - 优先级总结：**结果文件（taskId 匹配）> stream 快照（live / replay）> Ref**。
  - **journal 不再产出 agent 行**（v2 的 `ApplyJournal` 与 R3c 删除）：journal 行只有 `key / agentId / label / phase`，没有 CC 的 `index`
    （实测 `wf_3c5f7769-1b7/journal.jsonl` 中 0 处），行只能用合成 index，首张 live 快照整体替换时会留下客户端删不掉的幽灵行（§6.1）。
    journal 现在只用于 L2 的 phase 标题回填（header 级，不涉及行身份）与 §6.2.2 的 per-agent 结果查找。
- **Copy-on-write**：快照帧 → 重建该 workflow 的 `Agents`（O(agents)，400 行约 60KB 分配，
  每次结构变化一次；字符串经 §4.3 的记忆化，原串未变不重算）；只带 description/usage 的帧 → 只复制 header，`Agents` 切片与旧版
  共享（不可变）。
- **readLoop 上的总代价**（R2）：解码（§1.2.3）+ `Observe`（规范化、行重建）+ board `wake`（§5.8：逐行 diff、盖 rev 的副本、
  `byAgentID`、Summary）全部同步跑在 readLoop 上，预算以 §11.2 的 `BenchmarkObserve_Snapshot398` 为准，而不只是解码 bench。
- **回调**：`Process.SetOnWorkflowChange(fn func())`，`atomic.Pointer` 存储（同 `onCodeChange`，
  `process.go:121`、:420-426）。**回调只是唤醒信号，不携带 Set**：board 收到后自己去 Load 当前
  Set（§5.8），所以三个写者之间锁外回调的先后顺序无关紧要，旧 Set 无从覆盖新 Set。`internal/cli`
  不在 `no_late_setters` 名单内（`tools/lint-server-handlers/rule_late_setters.go:26` 只含
  session/cron/sysession/upstream）。不需要 catch-up 回调：board 绑定时直接 Load（§5.8）。
- **暴露方式**：`Process.Workflows() *workflow.Set`。session 层通过可选接口类型断言获取
  （`workflowNotifier`），仿 `codeChangeNotifier`（`managed_code_change.go:11`）；**不**给
  `processIface`（`managed.go:149`）加方法，免得波及 `testutil.TestProcess` 与 facet pin。

### 5.3 内存上限

| 项 | 上限 | 超出处理 |
|---|---|---|
| 每 Process 跟踪的 running workflow | 16 带行；第 17-32 个只记 header（`Degraded=too_many`）；> 32 忽略 | 超出 32 的 task 不建条目，计数 `workflow_untracked`（expvar）；header-only 条目照常 running，**从不**被合成终态 |
| 每 Process 保留的终态 workflow | 5（按 EndedAt LRU） | 从 Tracker 淘汰；board 的 wake 把它移入 `retained`（§5.8），不会从面板消失 |
| **每 board 的非终态条目**（live + retained，含 snapshot_stale / unknown） | 16（**只裁 retained**） | live + retained 超过 16 时，从 retained 里按 unknown → snapshot_stale / Ref 来源 → 其余、同类 `LastObservedAt` 最旧先**删除**（连带 `last` / 缓存 / 索引），直到满足或 retained 删空；**不**合成 interrupted（被裁不是终态证据），live 条目从不因此被裁或标终态（v3 会让 live > 16 时状态来回翻转、误播终态） |
| 每 workflow 的 phase | 200（按 index 升序保留） | 超出丢弃、计数，`Degraded=too_many`；所属 phase 被丢的 agent 行照常保留，客户端归入"其余 phase"组；HTTP 与 WS 都继承此上限 |
| **每 board 的终态条目**（live 与 retained **合并**计） | 5（按 EndedAt LRU） | 淘汰时同时删除 `last[task]`、结果缓存、journal 索引；Ref 从这个有界集合派生（≤ 21 条） |
| 每 workflow agent 行 | 2000 | 计数照算，行截断，`AgentsCapped=true` |
| 每 agent 历史 agentId | 8 | 丢最旧 |
| 每终态 workflow 的结果缓存（§6.2.1） | result 16KB + logs 合计 64KiB（≤ 200 行 × 500 runes，超出丢最旧） | ≤ ~80KB；不缓存 per-agent resultPreview |
| 每 workflow 的 journal 索引（§6.2.2，按需建） | agentId → 偏移，≤ 2000 项 × ~40B ≈ 80KB；journal 文件 ≤ 64MiB | 超过文件上限不建索引，per-agent 结果返回空 |
| 每 board 的首行缓存（§6.2.2、§8.2） | 按 `(agentId, dev, ino)` LRU 64 项（sessionId、agentId、≤ 4000 runes prompt） | ≤ ~800KB；首行永不变化，淘汰即可 |
| 后台磁盘 I/O（RunDir 解析、stat、结果文件、R3，§5.8 "I/O 派发"） | 每 board 在途 ≤ 2；全局 `workflowIOSlots = 8` | 在途 > 30s 记为卡死但**继续占全局槽位**，每 board 至多补发 1 个；挂死的文件系统最多卡住 8 个 goroutine / fd |
| 字符串 | agent：label/title/phaseTitle 120、model 64、last tool 64、last tool summary 200、error 400；workflow：Name 120、Description / Current / NotifySummary 200、RawState / RawStatus 32 runes | 先 `RedactSecrets` 再 `textutil.TruncateRunes`（`textutil/truncate.go:19`），按原串 maphash 记忆（§4.3） |

估算：一行 Agent 结构体 + 字符串 ≲ 600B，398 agents ≈ 240KB 上界、典型 ≈ 80KB。一个 workflow 的行最多同时有三份：
Tracker 当前版、CoW 期间的旧版、board 盖过 Rev 的上一发布版（盖 rev 需要自己的副本，因为 Tracker 的行不可变）。
典型 session（1-2 个 400 行的 running + 5 个终态、其中 1-2 个有缓存）稳态 < 2MB；病态上界（21 个条目各 2000 行、各三份）
约 75MB，靠 §5.3 各项上限兜住而不是靠估算。快照原始 `[]WorkflowItem`（~286KB）在 `Observe` 返回、且 §5.2 的 defer 清掉
`readEventBuf` 槽之后才成垃圾。

### 5.4 不进 event ring

- 快照、瘦身后的 Agent 列表**都不**写入 EventEntry；EventEntry 只做 §5.10 的 TaskType 标记与
  §9 的瘦身/合并。
- `ring.EventLog` 保持 wire 无关、无磁盘 I/O（`eventlog.go:41-45`、`eventlog_query.go:138-142`），
  Tracker 不注册 ring 回调（`SetOnAgentTaskDone` 单订阅者，`eventlog_agents.go:351-377`）。
- `EventEntry` 新增字段只限 additive omitempty（`clievent/types.go:105-111`，无需
  `SchemaVersion` bump）；`TaskType` 已存在，PR-7 只是给 workflow progress 条目也填上它。

### 5.5 多个并发 workflow 与 resume

- 一切按 `task_id` 分桶；同一帧只影响一个 task。`Set.Workflows` 中 running 在前（按
  StartedAt），终态按 EndedAt 倒序。
- **resume**（`resumeFromRunId`）产生**同 runId、新 task_id** 的 workflow，视为新条目。后果：
  - 同一 run dir、同一 `journal.jsonl`、同一 `wf_<runId>.json` 路径被前后两个 task 共享；
  - 续跑期间盘上的 `wf_<runId>.json` 属于**旧** attempt（`taskId` 为旧值、终态）。所有读结果
    文件的路径（§5.6、§5.9 R3b/R4、§6.2.1）都要求 `file.taskId == Workflow.TaskID`，否则视为
    文件不存在；
  - journal 对续跑而言是两次 attempt 的混合（续跑 agent 以同 key 复用之前的 result）。journal 已不产出 agent 行（§5.2），
    §6.2.2 按 agentId 查 result 行时取该 agentId 的最后一条，不受混合影响。
- adopt / background-fork 的 `paused` 占位任务**沿用同一 task_id**，不产生新条目，只是 status
  在 `paused` 与 `running` 之间变化。
- 前端按 `run_id` 把同 runId 的旧条目折叠为新条目下的"已续跑"子行（Q9）。

### 5.6 终态处理

1. `task_updated.patch.status ∈ {completed, failed, killed}` 是**最早**的终态信号（探针
   行 15 先于行 16）：立即置终态、`EndedAt = patch.end_time`、结构变化。
2. `task_notification`：确认终态（若尚未终态则以其 `status` 置终态，`stopped` → `killed`），
   记录 `NotifySummary`；仅当 `!ResultLoaded` 时用 `usage` 覆盖总 tokens/tool uses/duration。
3. 置终态时，仍为 queued/running 的 agent 规范化为 `stopped`（CC 终止后不再发快照，
   §1.2.2）。
4. **终态后读一次结果文件**：Tracker 进入终态（任一来源）且 `RunID` 已知时，board 经 I/O 派发（§5.8）异步
   读取 `<sess>/workflows/<runId>.json`（路径来自 board 的 RunDir 解析，§8.1；RunDir 尚未解析完成时排在解析任务之后，不另起；经 root 锚定的
   `osutil.OpenRegularIn` 打开，≤ 16MiB，
   §10；只解 §6.2.1 的瘦结构），经 `ApplyResultFile`（taskId 校验）覆盖 agent 行与 totals。文件尚未出现 → 交给下述 sweeper 重试。
5. 终态之后同 task 的 `task_progress` 一律忽略（防御；CC 本身不再发）。
6. **丢失终态的兜底**：shim 写通道满时整帧丢弃，仅 Debug 日志（`internal/shim/server.go:443-457`），
   naozhi 不检查 seq 连续性（`process_readloop.go:318`）。快照丢了下一张自愈，终态帧丢了
   会永远 running。兜底：
   - (a) **进程结束时按结束原因区分**。board 经 `bookProcessEnd` 的同一个 `SetOnEnd` 回调（单槽，见 §5.8）拿到
     `cli.ProcessEnd`（`process_end.go:14-32`）：
     - `!end.Detached && !end.ShimLive`（cli_exited、Kill / Close、watchdog 超时、shim 也已不在）——CLI 确实没了，workflow
       随之死亡：仍 running 的标 `interrupted`（随后若找到 taskId 匹配的结果文件则以文件为准）；
     - `end.Detached`（naozhi 优雅退出时 `Detach`，`process.go:386`）或 `end.ShimLive`（shim EOF / 读错 / oversize，
       `process_readloop.go:244-262`，shim 仍活着）——**只是 naozhi 这一侧断开**，CLI 与 workflow 还在 shim 里跑，reconcile loop
       每 30s 会重接同一个 CLI（`router_shim.go:113-130`、`main.go:238`）。此时**不**合成终态：保持 running，置
       `Degraded=snapshot_stale`，冻结 `LastObservedAt`；由重接后的新 Tracker（replay 种子）、(b) 的结果文件 sweeper 或 R4 收敛。
       v2 在这里一律标 interrupted，会在重接前的窗口里发布假终态（`#sr-announce` 播报"已终止"、agent 置 stopped、30s 存盘写进 Ref），
       重接后又翻回 running。
   - (b) **workflow sweeper**：挂在 `startCleanupLoop` 既有的 `saveTicker`（30s，
     `sessionSaveInterval`，`router_core.go:143`、`router_cleanup.go:540-553`）分支上，在
     `r.saveIfDirty()` 旁调用自由函数 `sweepWorkflowBoards(r.ss, time.Now())`：在 `r.ss.View` 里收集各 session 的 board 指针，
     出锁后逐个 `b.sweep(now)`。**不**新增 Router 方法、参数里也不出现 `*Router`——`router_budget_test.go` 把 Router 方法数钉在
     `routerMethodBaseline = 98`（现状恰好 98），带 `*Router` 参数的包函数则计入 `routerTypeRefBaseline = 5`，两者都是
     ratchet 台账指标。**不**挂在 `cleanupTicker` 上——它默认每
     15 分钟才一次（`main.go:237` `cfg.ParseTTL()/2`，`defaultSessionTTL=30m`）。sweeper 只做内存
     判定，然后经 board 的 I/O 派发（§5.8）发起异步 stat / 读取，不阻塞 tick。**在途上限**：每 board ≤ 2、全局
     `workflowIOSlots = 8`；在途超过 30s 视为卡死（例如落在挂死的网络文件系统上）——它**继续占着全局槽位**，该 board 至多再补发 1 个，
     直到卡死的任务返回；返回的结果按 taskId 与条目代数复核后丢弃。v3 的"卡死即允许发起新的"会在挂死的 FS 上每 board 每 30s 泄漏一个
     goroutine 与 fd、没有上限；现在最多卡住 8 个。RunDir 尚未解析完成的条目不是候选。候选：
     - `IsUnsettled`、`RunID` 已知、`now − LastObservedAt ≥ 60s`、距上次 stat ≥ 60s；
     - 终态、`!ResultLoaded`、`RunID` 已知，且（`now − EndedAt ≤ 10min`，或 `Source=ref` 且本次进程生命期内尚未尝试过），距上次 stat ≥ 30s
       （第 4 步的重试；后一种是 R0 恢复的终态条目补行，§5.9）；
     - **R4**（§5.9）：`IsUnsettled`、board 已无存活进程（未绑定，或绑定进程已结束）持续 ≥ `workflowOrphanAfter = 90s`
       （3 个 reconcile tick，常量）、且本轮 stat 未命中 taskId 匹配的结果文件 → `interrupted`；
     - **R5**（§5.9）：board **有**存活进程，retained 中的 `IsRunning` 条目自本次 bind 起 ≥ `workflowUnclaimedAfter` 未被当前 Tracker
       报告过、且本轮 stat 未命中 taskId 匹配的结果文件 → `unknown`（`RawStatus="unclaimed"`）。

     stat 命中后读取并经 `ApplyResultFile`。taskId 不匹配（续跑中的旧文件）→ 视为未命中。
     实际延迟：丢失终态帧后 ≤ 60s（无观测阈值）+ ≤ 30s（tick 间隔）= **≤ 90s**。
   - Tracker 归属的 workflow 由 Tracker 合并；进程已不在、只剩 board `retained` 的条目由 board 用
     同一个纯函数（`workflow.MergeResultFile`）合并，保证优先级规则只有一处实现。
   - 由 R4 或 (a) 合成的 `interrupted`、由 R5 合成的 `unknown` 都可逆：之后若某个新绑定的（或当前的）Tracker 带回同一 task_id，进程侧优先覆盖回 running
     （§5.8 合并规则）。客户端对每个 `(task_id, 终态 status)` 至多播报一次（§7.7），翻回再终止不会重复播报同一终态。

### 5.7 容错解析与优雅降级

| 级别 | 触发 | 行为 |
|---|---|---|
| L0 | 正常 | 全部功能 |
| L1 | 某些项的**非身份**字段类型错（`WorkflowDecode=Partial`，且通过 §4.1.1 的身份复核） | 坏字段置零，计数 `workflow_items_partial`（expvar），`Degraded=decode_error`；其余正常 |
| L1' | 条目本身或身份字段（type/index/phaseIndex/agentId/state）类型错，或 Partial 帧身份复核不过（按 Failed） | **保留上一版的行**，只更新 header；计数 `workflow_items_identity`，`Degraded=decode_error` |
| L1'' | 快照行的 envelope 超过 naozhi 的 10MiB 行上限（§1.2.3 天花板，约 4k-7k agents） | readLoop 的 oversize 分支（`process_readloop.go:189-195`，`line` 里留有前 ~10MiB）在跳过之前窥视前 1KiB：含转义形态的 `\"subtype\":\"task_progress\"` 且能截出 `\"task_id\":\"<id>\"`、该 id 是已知 workflow → `Tracker.NoteDropped(id, now)`：**保留上一版的行**，置 `Degraded=snapshot_dropped`（chip "明细过大，已停止更新"），计数 `workflow_lines_oversize`；下一张被接受的快照清除该标记（快照只增不减，实际上通常不会再出现）。截不出 id 只计数。不冻结 `LastObservedAt`（CLI 仍活着、帧仍在到达） |
| L2 | `workflow_progress` 整体失败 / 缺失 / 被 withhold | header 退化为 task_started + description + usage（名称、当前 "phase: label"、tokens、tool uses、elapsed），`Degraded=no_snapshot`；phase 标题若是 `phase N` 且有 run dir，用 journal 的 `phase` 字段 / meta.json 的 `workflowPhase` 替换 |
| L3 | 连 task_started 都没见到（replay 被淘汰） | 仅当 `IsWorkflowTask` 为真（见下）才建条目；名称回落 `summary` |
| L4 | CC 完全改协议 | 无条目，dashboard 与今天一致；`workflow_decode_errors` 计数可观测 |

**`workflow.IsWorkflowTask` 判定**（L3 建条目、§5.2 的 `ev.WorkflowTask`、PR-7 的 TaskType 标记、
§9 共用；顺序求值）：

1. 帧带 `subagent_type` → **否**（agent 任务的反证）。
2. 曾见该 task 的 `task_started.task_type == "local_workflow"` → 是。（v2 还写了 `background_tasks_changed` 的
   `task_type`：可 Event 不解它的 `tasks:[{task_id,task_type}]` 数组，SeedFromReplay 的前缀门控 `subtype":"task_` 也不收这个
   subtype，那个分支永远不会命中，删除；不为它加定向解码——task_started 与规则 3 已经够用。）
3. 本帧带 `workflow_progress` 键（`WorkflowProgress != nil`，或 `WorkflowDecode == Failed`：键在但不是数组）→ 是。CC 只给 workflow 帧附这个键（非 workflow 调 `Une` 时该值为 undefined，序列化时省略）。
4. 已知 `WorkflowLaunch.TaskID` 或 session 侧 Ref 里有该 task_id（board 在绑定时把 Ref 的
   task_id 集合交给 Tracker）→ 是。
5. 其他 → **否**。`summary` 不是判据；task id 形态 `^w[0-9a-z]{8}$` 也不在这里用（误判会造出钉住
   保活的幻影 running workflow），只用于 §5.10 的遗留 InjectHistory 过滤。

规则：只认 `index` / `phaseIndex` / `agentId` 做身份，不以 title 做键；所有字段视为可选；
只有 `WorkflowProgress != nil && WorkflowDecode != Failed` 才替换 agent 行（键缺失或 `null` 不
清空；身份类错误按 Failed，§4.1.1）；未知 status/state 透传原值不报错（截 32 runes）。

### 5.8 Session 层：Board、快照、持久化、保活

新文件 `internal/session/managed_workflow.go`：

```go
type workflowBoard struct {
    mu        sync.Mutex
    epoch     string                         // 创建时随机；board 随 respawn/rename 以指针携带
    projectsRoot string                      // 创建时传入（HistoryIO.projectsRoot，§8.1），不变
    workspace string                         // 每次 bind 由 bookWorkflows 传入 s.Workspace()；R0 时取恢复出的 workspace
    proc      workflowNotifier               // 当前绑定且尚未结束的进程；nil = 无
    procGone  int64                          // 失去存活进程的时刻（R4 计时，§5.6(6b)）；有进程时为 0
    bindAt    int64                          // 本次 bind 的时刻与 replay 是否绕回（R5 计时，§5.9）
    bindWrapped bool
    resolve   map[string]resolveState        // per-task RunDir 解析：来源元组、代数、结果 / 在途（下文"RunDir 解析"）
    io        ioDispatch                     // 有界后台 I/O：在途 ≤ 2，共享全局 workflowIOSlots（§5.6(6b)）
    applied   uint64                         // 已应用的 proc Set.Version
    retained  map[string]*workflow.Workflow  // 不由当前进程持有的条目：旧进程的、Tracker 已淘汰的、Ref 恢复的（有界，§5.3）
    last      map[string]wireState           // 上次发布的 wire 态：Version、RowsGen、源 Agents 切片头、盖过 Rev 的行
    cache     map[string]*resultCache        // 终态结果缓存（§6.2.1），随条目淘汰
    jindex    map[string]*journalIndex       // journal agentId → 偏移（§6.2.2），按需建，随条目淘汰
    cur       atomic.Pointer[workflow.Published]
    summaries atomic.Pointer[[]workflow.Summary]
    running   atomic.Bool                    // 无锁访问器的来源，发布时更新
    lastObs   atomic.Int64
    subs      …                              // buffered(1) 通道表
    pending   atomic.Uint32                  // 待发通知位：structural | countOnly
    notifyAt  time.Time; notifyTimer *time.Timer // sessions_update 的唯一发射点（timer 自己的 goroutine）
    onStructural, onCount func()             // bookWorkflows 传入的闭包（见下文"通知"）
}
```

- **锁序（硬规则）**：表锁（`r.ss`）→ `b.mu` → `Tracker.mu`。
  - 允许在 `r.ss.Update` 事务里调 `bind` / `procEnded`（取 `b.mu`）：三个 bind 点里 `installFreshSession`
    （`router_lifecycle.go:665` 的事务 → :798）与 rename（`router_rename.go:27` 的事务 → :117）本来就在事务内。
  - **禁止持 `b.mu` 时取表锁**。表锁是普通 `t.mu.Lock()`、不可重入（`sessiontable/tx.go:31-35`）。v2 在 b.mu 内直接
    `r.ss.Update(markChanged)`：事务内 bind 当场 wake 出结构变化（旧 proc 的 running 条目被折叠、Ref 条目首次发布、
    Tracker 已领先 `applied`）就会在同一 goroutine 上二次加锁、自死锁；readLoop 的 wake（b.mu → 表锁）与另一个
    goroutine 事务内的 bind（表锁 → b.mu）还构成 ABBA。
  - Tracker 在 `Tracker.mu` 之外调 wake（§5.2）；board 持 b.mu 时只做 `proc.Workflows()`（原子 Load）与 bind 时
    下发 task_id 集合（取 Tracker.mu，顺序合规）。
  - 读者——`/api/sessions` 快照、Hub、HTTP，以及事务内 `evictOldest(tx)`（`router_capacity.go:137`）/ Cleanup /
    ReleaseIdleProcess 里的 `workflowPinned`——只做原子 Load，不取 b.mu。
  - 磁盘 I/O 从不在 b.mu 内，**也从不同步跑在 readLoop 或表事务里**——包括 RunDir 解析（v3 在发布时同步调 `ResolveWorkflowRunDir`，
    它的 EvalSymlinks / `sameFileAncestor` 的 Lstat 祖先遍历就落在 b.mu、readLoop 与 `r.ss.Update` 里）。锁内只记"需要做什么"，
    由 I/O 派发起 goroutine 在锁外做，结果回到 b.mu 内复核后应用（见下文"RunDir 解析"）。
  - 测试：在 `r.ss.Update` 回调里对一个 Tracker 含 running workflow、且已领先 `applied` 的 proc 调 bind，必须返回且随后
    恰好一次 structural 通知（v2 设计下此测试死锁）；`-race` 下 readLoop wake 与事务内 bind 交错 10k 次无死锁。
- **通知**：wake / bind / procEnded / sweep 在 b.mu 内只算出"结构变化 / 仅计数变化"，置 `pending` 位并 arm
  `notifyTimer`（结构 → `Reset(0)`；仅计数 → 按下文的 30s 窗口）。timer 回调跑在 timer 自己的 goroutine 上，
  **不取 b.mu**（原子交换取走 `pending`），调用 `bookWorkflows` 传入的闭包：
  - structural：`func(){ r.ss.Update(markChanged); r.notifyChange() }`（与 `bookCodeChanges` 的闭包相同；markChanged
    是因为 Ref 要落盘）；
  - countOnly：`r.BumpVersion`（`router_core.go:743`：`BumpGen` + `notifyChange`，不置 dirty）。
  闭包在既有 router 方法内构造，不新增 Router 方法（§5.6(6b) 的预算说明）。
- **合并与发布**（`wake(p)`，在 `b.mu` 内）：
  1. `p != b.proc` → 忽略（旧进程 / 被放弃的进程 / 已结束的进程）。否则 `set := p.Workflows()`；
     `set.Version <= b.applied` → 忽略（已应用过更新的）；否则 `b.applied = set.Version`。
  2. **淘汰接管**：`b.last` 里有、`set` 里没有、也不在 `retained` 里的 task——Tracker 按每 Process 5 个终态 LRU 把它淘汰了——
     把上次发布的条目移入 `retained`（万一它不是终态，标 `snapshot_stale`）。v2 只在 bind 时写 retained，这类条目会从
     Published / Summary / HTTP 直接消失。
  3. 合并 `set.Workflows` 与 `retained`：按 TaskID 去重，**进程侧优先**（覆盖 retained 中同 task_id 的条目，包括 R4 / §5.6(6a)
     合成的 interrupted、snapshot_stale 与 R5 的 unknown）；再按 §5.3 的 board 上限裁剪（非终态 ≤ 16，**只从 retained 里删**，live 条目不参与裁剪、
     不被标终态；终态 5，live 与 retained 合并按 EndedAt LRU），被裁掉的条目连同 `last` / `cache` / `jindex` / `resolve` 一起删除。代价
     O(live + 有界 retained)，不随 session 寿命增长。
  4. **分配 wire 版本**：对每个 task 与 `b.last[task]` 比较——
     - `Agents` 切片与上次源切片同一底层数组且长度相同（CoW 共享）→ 行沿用上次的 wire Rev，不逐行比较；
     - 否则逐行（按 index）用 `agentEqualIgnoringRev(a, b)` 比较：相同则沿用上次 Rev，不同或新增则盖
       `newVer = last.Version + 1`。`Agent` 含 `PrevAgentIDs []string`，**不是**可比较结构体（v2 写的 `==` 编不过），
       所以这是手写函数：标量字段逐个 `==`、`PrevAgentIDs` 用 `slices.Equal`；单测以 `reflect.TypeOf(Agent{}).NumField()`
       钉住字段数（新增字段而忘了在这里比较即红），并测"只有 PrevAgentIDs 变化（重试换 id）也推进 Rev"；
     - **行集收缩**：上次发布有、这次没有的 index → 该 task `RowsGen++`，全部行盖 `newVer`。RowsGen 变化的 task，推送端对每个
       订阅重发 full、HTTP 响应带新 RowsGen，客户端丢弃本地行（§6.1）——"行不会被删除"因此成为每个 RowsGen 内由服务端保证的
       不变式，而不是对 CC 行为的假设；
     - header 或任一行变化 → 该 task 的 `Version = newVer`；否则维持不变（不发布该 task）。
     盖 Rev 要 board 自己的行副本（Tracker 的行不可变），即 §5.3 估算里的"board 上一发布版"。

     这样无论来源怎么换（Ref → replay 种子 → live、shim 重连换 Tracker、retained 被 board 改为
     interrupted / snapshot_stale / result_file），wire 版本与行 Rev 都单调，且内容没变的行不会重发。
  5. structural 由 board 判定：Published 的 task 集合、任一 status、RunID 任一变化。
  6. 构建 `Published{Epoch, Workflows, byAgentID}` 与 `summaries`：agentId 集合（含历史 id）未变时复用上一版的
     `byAgentID`（只读共享），否则重建；每个条目的 RunDir 取自 `b.resolve[task]` 的已解析结果（Tracker 发布态里恒为 ""）；Store；更新
     `running` / `lastObs`；唤醒订阅者（非阻塞写 buffered(1)）；按"通知"置位。
  7. **RunDir 解析只登记、不执行**：对每个条目求来源元组 `(LaunchTranscriptDir, SessionID, RunID, b.workspace)`，与 `b.resolve[task]`
     记录的不同（新条目、RunID 首次获知、R3a 找到了目录……）→ 代数 +1、清掉旧结果、标"待解析"，交给 `b.io`。`b.io` 在 b.mu 内只做记账
     （槽位够就 `go` 一个任务，不够就排队，同一 task 至多一个在途），**不做任何系统调用**，所以 wake 在 readLoop 上、bind 在表事务里执行都安全。
- **RunDir 解析**（I/O 派发的一种任务；其余是 stat、结果文件读取、R3 与 journal 索引）：goroutine 在锁外调用
  `claudefs.ResolveWorkflowRunDir(b.projectsRoot, src)`（ProjectDir 由 `claudefs.ProjectSlug(workspace)` 纯字符串求出，§8.1），然后取 b.mu：task
  仍在、来源元组与代数都未变 → 写入 `b.resolve[task]` 的结果并重新发布（RunDir 不上 wire，**不推进 version**、不发通知）；否则丢弃。解析失败
  （ok=false）也记下，元组不变就不重试。依赖 RunDir 的工作都等它完成：终态后的结果文件读取（§5.6(4)）排在它之后；sweeper 与 R3 跳过未解析的
  条目；drill-in 返回 202 pending（§8.2，与"transcript 尚未落盘"同一语义）；HTTP 不返回 result / prompt，置 `result_unavailable`（§6.2）。
  测试：用计数型 fake resolver 断言在 `r.ss.Update` 内 bind、在 readLoop 上 wake 都**零次**同步调用；resolver 阻塞在 channel 上时 wake / bind
  照常返回、其他 board 不受影响；来源元组在解析途中变化时旧结果被丢弃。
- **绑定**：`bookWorkflows(s, proc, onStructural, onCount)`（内部调 `s.workflows.bind(proc, s.Workspace())`），在三处与 `bookProcessEnd`
  **同位**调用：`router_lifecycle.go:798`（`installFreshSession` 内；此时 `s.workflows` 已是携带来的 board，见下文"携带"）、`router_shim.go:489`（**`commitShimReattach` 成功之后**，不是
  `bookCodeChanges` 所在的 :395——被放弃的 reattach（`reattachReplaced`/`reattachSendInFlight`
  会 `proc.Close()`）不得往一个从未持有该 proc 的 session 的 board 里发布）、`router_rename.go:117`。
  三处都**紧挨在 `bookProcessEnd` 之前**调用：若进程在绑定前已经结束，`bookProcessEnd` 里的 `SetOnEnd` 会同步投递 ProcessEnd，此时
  board 已绑定该 proc，`procEnded` 才能把它解绑并开始 R4 计时；顺序反过来，board 会绑着一个已死的 proc、永远等不到结束信号
  （endHook 只投递一次，`process_end.go:34-46`）。
  `bind(proc)` 在 `b.mu` 内：
  0. `proc == b.proc` → **no-op**（rename 沿用同一个 proc，`router_rename.go:105-118`；board 也以指针携带，见下）。
  1. 若仍绑定着另一个旧 proc（尚未收到它的 ProcessEnd）：把它最后一次已应用的条目折入 `retained`，仍 running 的标
     `snapshot_stale`、**不**标 interrupted——此刻不知道它的 CLI 是否已死；它的 ProcessEnd 随后到达时由 `procEnded` 按原因处理
     （届时 `proc != b.proc`，只更新 retained 里来源是它的条目——retained 条目记录来源 proc 的身份）。新 Tracker 若带同 task_id，进程侧优先覆盖。
  2. 记 `b.proc = proc`、`b.applied = 0`、`b.procGone = 0`、`b.workspace = workspace`、`b.bindAt = now`、`b.bindWrapped = proc 的 replay 是否绕回`
     （§5.9 判据，SeedFromReplay 记在 Set 上），把 Ref / retained 的 task_id 集合交给 Tracker（§5.7 规则 4）；
  3. `proc.SetOnWorkflowChange(func(){ b.wake(proc) })`，然后立即执行一次 `wake(proc)`——
     bind 之前 readLoop 已经喂进 Tracker 的帧由这次 Load 拿到，无窗口。在事务内执行也安全：wake 不取表锁，通知由 timer 异步发出。
- **进程结束**：`SetOnEnd` 只有一个槽（`process_end.go:44-56`），已被 `bookProcessEnd` 占用，所以不另设回调，而是让
  `bookProcessEnd` 的回调扇出：`func(end){ s.costAcct.onProcessEnd(s, end, claudeDir); s.workflowBoard().procEnded(proc, end) }`。
  该回调可能在 `SetOnEnd` 内同步执行（进程在绑定前已结束，`process_end.go:44-46`），也就可能在表事务内——`procEnded` 只取 b.mu，
  符合锁序。`procEnded(proc, end)` 在 b.mu 内：若 `proc == b.proc`，先按 wake 的步骤应用该 proc 的**最终** Set（readLoop 已退出，Set
  不会再变；否则读到终态帧而 wake 尚未跑到的 workflow 会被误标），再把仍 running 的条目按 §5.6(6a) 处理——
  `!end.Detached && !end.ShimLive` → `interrupted`；否则 → `snapshot_stale`（保持 running、冻结 LastObservedAt）——并折入 retained；
  若 `proc == b.proc`，再置 `b.proc = nil`、`b.procGone = now`。此后该 proc 的 Tracker 上迟到的磁盘合并不再影响 board（wake 因
  `p != b.proc` 被忽略），retained 条目改由 board 自己的 `MergeResultFile` 合并（§5.6）。
- **携带**：rename / respawn 时**以指针**把 board 移到新 ManagedSession，订阅者与 epoch 跟着走（不同于 `codeChanges` 的值拷贝）。
  **携带必须先于绑定**——bind 记下的是"哪个 board 持有新 proc"，先绑后换会让新 proc 绑在一个随即被丢弃的 board 上：
  - **respawn**：v3 写的 `fresh.workflows = old.workflows` 在 `router_lifecycle.go:671`（`installFreshSession` 返回之后），而 bind 在
    `installFreshSession` 内的 :797-798。新 proc 被绑到临时的空 board 上，随即被旧指针覆盖；携带来的 board 的 `b.proc` 还是旧 proc 或 nil，
    收不到新 proc 的任何 wake，R4 最终把仍在跑的 workflow 标成 interrupted。现在 board 走 `respawnSnapshot`（`respawn_snapshot.go:60-104`，
    与 `codeChanges` 同一条路）：`snapshotRespawn` 里 `snap.workflows = old.workflowBoard()`，`rereadSameEntry` 在提交事务内重读一次；
    `installFreshSession`（:722）增参数 `board *workflowBoard`，在 `bookCodeChanges` / `bookWorkflows` / `bookProcessEnd`（:797-798）**之前**
    `s.workflows = board`，`board == nil`（`old == nil`：首次 spawn、`/new`）时才 `newWorkflowBoard(r.hist.projectsRoot)`。它仍是纯状态变更、无 I/O。
  - **rename**：`fresh.workflows = old.workflows` 放在 `router_rename.go:78`（`setCodeChanges` 旁）一带，早于 :117 的绑定，顺序本来就对；
    同一 proc 重复 bind 是 no-op（bind 第 0 步）。
  - **R0**（`router_restore.go`）：`newWorkflowBoard(projectsRoot)` 后写入 Ref 条目，workspace 取恢复出的 `sess.Workspace()`。
  测试钉住：respawn 之后 `s.workflows` 与旧 session 的是同一指针、该 board 的 `b.proc` 是新 proc、喂给新 proc 的帧出现在它的 Published 里；
  rename 同理；`/new` 是新 board（新 epoch）。
- **访问器**：`Running() bool`（Published 中存在 `IsRunning` 的条目，§4.2）、`LastObservedAt() int64`、
  `Workflows() *workflow.Published`、`WorkflowAgent(agentID) (workflow.AgentLoc, bool)` **全部无锁**（原子 Load），
  事务内的 `evictOldest(tx)` / Cleanup 经 `workflowPinned` 调用它们不会等 b.mu；`Subscribe() (<-chan struct{}, func())`
  取 b.mu，只由 Hub 在锁外调用（每订阅者 buffered(1)，与 ring 订阅同语义，`eventlog_subscribe.go:13,25-35`，自然合并唤醒）。
  命名避开 `Get*`（`getter_surface_test.go:48`）；不是 `SetOn*` 形态，不触发 `no_late_setters`。
- **快照**：`sessionview.SessionSnapshot` 增 `Workflows []workflow.Summary `json:"workflows,omitempty"``
  （放在 `Subagents` 旁，`sessionview/snapshot.go:58`），在 `managed_query.go:207` 附近一次
  atomic Load 填充；同 `MeteringUsage/CodeChanges` 的只读共享契约（`snapshot.go:74-88`），满足
  #411 O(1)。`internal/server/sessions_shape_test.go` 的 `allowed` 集合加 `workflows`；
  `internal/dashboard/session/testdata/rest.schema.json` 含 `sessionview.SessionSnapshot` def
  （`restResponses["sessions"]`），须 `go test ./internal/dashboard/session -run TestRESTSchema
  -update-rest-schema` 重生成，否则 `TestRESTSchema_IsGenerated` 红。只放 running + 最近 3 个终态（与面板显示的集合一致，§7.2）。
- **sessions_update 节流**：
  - 结构变化（新增 / 移除 workflow、终态、RunID 首次获知）→ notifyTimer `Reset(0)` → `onStructural`
    （`r.ss.Update(markChanged); r.notifyChange()`，在 timer goroutine 上，见"通知"）。
  - 仅计数 / 当前 phase 变化 → 每 session **至多每 30s 一次**（`workflowSummaryMinInterval`，常量），
    trailing edge：窗口内有变化则窗口结束时补发一次 → `onCount` = `r.BumpVersion()`。**必须推进 gen**：WS 连着时
    `fetchSessions` 在 `stats.version`（就是表的 gen，`router_core.go:793-797`）不变时提前返回（`session_list.js:96-103`），
    sidebar 重绘与 `sessionsAppliedHooks` 都在这个检查之后（:275-286）；而 `notifyChange` 只广播、不推进 gen
    （`router_core.go:707-711`）。v2 只发通知，每个 tab 会重拉一次、再把新的 `workflows[]` 丢掉，徽标与兜底刷新永远不动。
    也不能用 markChanged：它置 dirty，会每 30s 为每个 running workflow 重写一次 sessions.json。`BumpVersion` 的 godoc
    写的正是这种"只给 UI 的刷新信号"（同 `server_loops.go:94` 的先例）。
  - 取舍：每次计数型 sessions_update 让每个 tab 重拉 `/api/sessions`（既有 broadcast debounce 合并），而且因为 version
    前进，**每个 tab 都会整块重绘 sidebar、重跑 main-state reconcile 等 applied hooks**。代价 ≤ （有 running workflow 的
    session 数）/ 30s 次每 tab；换来 sidebar 徽标与 Q12 的 activity 文本至多 30s 陈旧。逐 agent 完成就通知会是
    400-agent workflow 数百次重拉，不可取。
  - 同一信号也是 §6.1 无推送场景（suspended、订阅过期）的面板兜底刷新来源。
- **持久化 Ref**：`store.go` `storeEntry` 增 `Workflows []workflow.Ref `json:"workflows,omitempty"``
  （仿 `CodeChanges`，`store.go:62-64,174,199`），从 board 的有界集合派生（非终态 ≤ 16 + 终态 5，≤ 21 条、每条 ~200B；
  `LastObservedAt` 早于 `now − workflowPinMax` 的 unknown 条目不写，所以 R5 判过的条目不会无限期地随每次存盘回到 Ref）；含 `last_observed_at`。旧版 naozhi 读到未知键会忽略，降级安全。`Ref.Name` 来自已脱敏的 `Workflow.Name`。
- **保活**（不改 `turnOutstanding` 本身——它还被 `scratch.go:225` 调用）：
  - 新谓词 `s.workflowPinned(now) = board.Running() && now − board.LastObservedAt() < workflowPinMax`
    （`workflowPinMax = 6h`，常量，非配置；Q4）。`Running` 含 paused 与 snapshot_stale（§4.2）；snapshot_stale 期间
    LastObservedAt 冻结、R0 恢复的条目取 `Ref.LastObservedAt`，所以断联或恢复出来的条目不会无限钉住。
  - `ReleaseIdleProcess`（`managed_release.go:20`）：条件加 `|| s.workflowPinned(now)`——否则会关掉
    正在跑 workflow 的 CLI。
  - `Router.Cleanup`（`router_cleanup.go:278-305`）：`effective = max(effective, board.LastObservedAt)`；
    `workflowPinned` 时跳过 idle TTL 过期。stuck-running 判定不变（workflow 不让 parent 变 running）。
  - `ScratchPool.lastActivity`（`scratch.go:225`）：返回 `max(既有值, board.LastObservedAt)`，scratch
    从最后一次 workflow 观测起老化；`turnOutstanding` 不变，所以卡死的 running（丢了终态）不会让
    scratch 永不过期。
  - `evictOldest`（`router_capacity.go:137-150`）两遍：先在非 `workflowPinned` 的 idle session 中挑最
    旧；无候选时回退到全部 idle session（含 workflow 的）。因此 `takeoverHasSlot`（:117-133）的
    `evictable`（任一 idle）判定与驱逐实际行为一致，不必改；测试覆盖回退。
  - 已知遗留：shim 的 idle timer 不随 stdout 刷新（`internal/shim/server.go:216`、
    `server_client.go:226`），naozhi 断开超过 shim `idle_timeout`（默认 4h）仍会杀 CLI——属于
    shim 既有行为，本 RFC 只记录（§13 R7）。

### 5.9 重启重建顺序

```
R0  router_restore：storeEntry.Workflows（Ref）→ board.retained
      running 的标 Source=ref、Degraded=snapshot_stale；LastObservedAt = Ref.LastObservedAt（缺省取恢复时刻）；
      Ref.SessionID / RunID 重新过 IsValidSessionID / IsValidWorkflowRunID（后者在 PR-4 落地），不过即丢弃该条；b.procGone = 恢复时刻；
      终态条目没有行（Ref 不带 agents）：RunID 已知的，在 RunDir 解析完成后经 I/O 派发做一次 MergeResultFile（行 + 总计 + 结果缓存，
      按普通 board 版本推进发布）——否则没有 shim 的重启之后，展开恢复出来的终态 workflow 只有 header（sweeper 的终态候选要求
      now − EndedAt ≤ 10min，R3b 又只在重接时跑）；sweeper 的第二个候选为此放宽到"Source=ref 且尚未尝试过"（§5.6(6b)），HTTP 的缓存缺失路径
      调用同一个 board 方法（§6.2.1）
R1  ReconnectShimsCtx → SpawnReconnect（wrapper.go:586-642）:
      DrainReplay → proc 构造（Tracker 已在）
      → Tracker.SeedFromReplay(replays, proto)        ← 新增，startReadLoop 之前
      → reconnectVerdict(...)；结果为 unknown 时同步调用 session 层传入的 resolver 读 JSONL 尾（§5.10）
      → applyReconnectVerdict(verdict)                 ← 同时修 turn-neutral（§5.10）；startReadLoop 之前武装
      → startReadLoop
R2  router_shim.go：commitShimReattach 成功后 bookWorkflows → bind → 立即 Load 发布
      → board 合并 retained（Ref 提供被淘汰的 RunID/Name）；进程侧同 task_id 覆盖 snapshot_stale；版本按 §5.8 单调分配
R3  紧随其后（SetCwdForLinker 已于 :405-407 执行）由一个闭包经 board 的 I/O 派发启动一次异步磁盘对账（不新增 Router 方法，§5.6(6b)）；
    所有路径经 claudefs.ResolveWorkflowRunDir（§8.1），所有文件 / 目录经 root 锚定的 osutil.OpenRegularIn / OpenDirIn + 有界 ReadDir（§10）：
      a. 缺 RunID：OpenDirIn 打开 <ProjectSessionDir>/subagents/workflows/（被换成 FIFO / symlink 时立即报错、不阻塞），f.ReadDir(257) 列目录（> 256 项即放弃并计数，
         名称过 runID 正则），找包含快照中任一 agent-<agentId>.jsonl 的 run dir（agentId 随机 a+16hex，无歧义）
      b. 有 RunID 且 <sess>/workflows/<runId>.json 存在且 taskId 匹配 → ApplyResultFile（权威终态 + 总计）
      （v2 的 c：running 且无快照时读 journal 生成粗粒度行——删除，§5.2：journal 行没有 index；这种 workflow 只显示 header，
       Degraded=no_snapshot，下一张 live 快照 ≤ 10s 内到达，§1.2.2）
R4  无存活进程（shim 已死、重接失败或尚未重接）：由 30s sweeper 执行（§5.6(6b)）——
      taskId 匹配的结果文件存在 → 终态（board 侧 MergeResultFile）；
      否则 b.procGone 起 ≥ workflowOrphanAfter（90s，3 个 reconcile tick）→ interrupted（可逆：之后重接的 Tracker
      带回同一 task_id 即覆盖回 running）。v2 写的"否则 → interrupted"没有触发点；而 ReconnectShimsCtx 在 Discover 失败时只记 warn、
      reconcile loop 每 tick 重试（router_shim.go:230、main.go:238），启动时没接上的 shim 之后仍可能接上，所以要等几个 tick。
R5  **有**存活进程、但 retained 条目没人认领（v4 新增）：R4 只管"无存活进程"。重启后 shim 已死、90s 内又有消息 spawn 了新 CLI，bind 把
      procGone 清零，R0 恢复的 running 条目再无规则可收敛，每次存盘都以 running 写回 Ref；adopt / paused 路径还不写结果文件（§1.2.2）。
      由 30s sweeper 执行：retained 中 IsRunning 的条目，自 b.bindAt 起 ≥ workflowUnclaimedAfter 当前 Tracker 从未报告该 task_id，
      且本轮 stat 未命中 taskId 匹配的结果文件（RunID 未知视为未命中）→ unknown（RawStatus="unclaimed"）。
      workflowUnclaimedAfter：b.bindWrapped 为假时 90s——replay 完整，同一个 CLI 若还在跑它，SeedFromReplay 已经把它种进 Set；新 spawn 的
      CLI 没有 replay，也不可能在跑旧 workflow；为真时 10min——安静 phase 的帧可能已被挤出 ring，要给 live 帧留时间。
      取 unknown 而不是 interrupted：无法确认原 CLI 已死。unknown 不钉保活（IsRunning 为假），仍是 sweeper 候选（继续找结果文件），
      进程侧之后报告同一 task_id 即覆盖回来；LastObservedAt 超过 workflowPinMax 后不再写进 Ref（§5.8）。
```

`SeedFromReplay` 细节（单遍、有界）：

- **逆序**遍历 replay，先做廉价前缀门控：只看以 `{"type":"system","subtype":"task_` 开头或
  `{"type":"user"` 开头且含 `"async_launched"` 的行（其余行零解码；ring 至多 10000 行）。
- 对门控命中的行，从首个结构性 `"task_id":"` 处截出 id（值须匹配 `^[a-z0-9]{1,32}$`，否则
  回落完整解码）。每个 task 按**帧类**记"已取到最新一行"的格：带快照的 task_progress、不带快照的 task_progress（description / usage）、
  task_updated、task_notification，共四格（帧类由前缀与本行是否含 `"workflow_progress"` 判定，不需解码）。逆序下第一次遇到的就是该类最新的一行：
  - 该类的格已满 → **跳过解码**。四类帧都是"最新的覆盖旧的"：快照整体替换行，description-only 帧只更新 header，task_updated 的
    patch.status / end_time 与 task_notification 的 status / summary 都只取最新值。这一条对 running 的 workflow 同样生效（它没有终态帧，
    但不需要等终态格填满）；
  - `subtype":"task_started` 与 `async_launched` 行总要解码（名称、task_type、RunID、TranscriptDir 只在这里）。
  v3 只跳过快照行：ring 绕回后几乎全是小的 task_progress 行，每行仍要反射解码（实测 347B 的行 6.8-9.1µs / 440B，10k 行约
  70-90ms / 4.4MB，是 v3 所写预算的 3-4 倍）。现在解码次数为 O(task 数)：每 task ≤ 4 行 + task_started + launch 行。
- 不把 replay 帧写进 ring / persist（`router_shim.go:409-413` 的既有约束：replay 无时间戳；
  persist sink 最后才装，`router_shim.go:483-486`）。
- 已知：长 workflow 后 replay ring 几乎必然已淘汰 `task_started` 与 launch tool_result
  （10000 行上限被 progress 小帧撑满），所以 R0 的 Ref 与 R3a 的磁盘定位是必要的，而非锦上添花。
  ring 是否已绕回：首个 replay 的 `Seq > 1`（或 hello 的 `BufferSeqStart > 1`，
  `server_client.go:90-103`、`shim/protocol.go:32`，naozhi 目前未读）；绕回时才走 R3a 扫描，
  §5.10 的 `unknown` 判定也用它。

**重启时 replay 的合并预算**（三遍对同一 replay 的处理都要计入）：

| 遍 | 今天 | 本 RFC 后 |
|---|---|---|
| `SeedFromReplay` | 无 | 前缀门控 + 每 task 每帧类只解最新 1 行（1 张快照 + 3 个小帧）+ task_started / launch 行：≤ 32 × (~1ms + 5 × ~10µs)；更旧的行按 task_id 截取后跳过 |
| linker walk（`router_shim.go:416-450`） | 每行 `ReadEvent` 完整解码 | 只解以 `{"type":"system","subtype":"task_started"` 开头的行（PR-5） |
| `reconnectVerdict`（`wrapper.go:673-698`） | 逆序每行解码，遇首个非中性帧停 | 中性帧按前缀识别、不解码直接越过（PR-3）；只解真正的语义帧 |

这三遍**在既有的 DrainReplay 之上**合计新增：50MiB replay（~100 张大快照、其余是上万个小帧）每 session 解码 ≤ ~20ms（≤ 16 个 task）、
病态上界（32 个 task）~40ms，分配 ≤ ~5MB；不随快照张数、也不随小帧行数线性增长（v3 漏算了小帧：每行仍要解码，10k 行约 70-90ms）。这不是重连的总代价：`drainReplay`（`internal/shim/manager.go:541-560`）今天就对每个 envelope 做
`json.Unmarshal` 到 `ServerMsg.Line`（:508）并持有整个切片，按 §1.2.3 的 0.90-1.07ms / 1.04MB 每个 533KB envelope 计，
50MiB 约 ~100ms、~100MB 分配，且重连按 session 串行——这部分是既有行为，本 RFC 不改。总代价仍远低于
`shimReconnectTimeout=15s`（`router_core.go:126`）与 `drainReplayTimeout=20s`（`internal/shim/manager.go:517`）。
测试用解码计数与 `testing.AllocsPerRun` 断言新增的三遍，不用挂钟（§11.2）。后续可选（不在本 RFC）：让 shim 对门控外的行跳过
envelope 反转义，或下发预解码的 seq 索引。

### 5.10 linker 与 turn 判定修正

- **linker 排除**：`local_workflow` 加入三处排除：`process_readloop.go:663-665`、`router_shim.go:433`、
  `process_event_query.go:75-82`。InjectHistory（后者）的判定：
  - 条目带 `TaskType == "local_workflow"`（同批 `KindTaskStart`，以及 PR-7 起 workflow 的
    `KindTaskProgress` / `KindTaskDone` 条目也带，§5.4）→ 跳过；
  - **遗留条目**（PR-7 之前持久化、无 TaskType 的孤儿 `KindTaskProgress`：其 task_start 已被挤出
    500 条 `maxPersistedHistory` 窗口，`managed.go:18`）：同批无该 task 的 task_start 且 task_id 形如
    `^w[0-9a-z]{8}$`（CC 的 local_workflow 前缀，§1.2.2）→ 跳过。误判代价只是少跑一次本就会
    tombstone 的 Resolve。
- **`isTurnNeutralEventType(t)`（`wrapper.go:717`）改为 `isTurnNeutralLine(line) / isTurnNeutralEvent(ev)`**：
  `control_ack`，以及 `system` 且 subtype ∈ {`task_progress`, `task_updated`, `task_notification`,
  `background_tasks_changed`}。逆序 walk 先用行首前缀（`{"type":"system","subtype":"task_progress"` 等
  四个）识别中性帧并**不解码**直接越过（`task_started` 不是中性帧），找到真正的最后一个语义帧：
  - 前台 Agent 中途 → 越过后遇到 assistant tool_use → 仍判 midTurn（正确）；
  - 空闲期后台 workflow → 遇到 result → 判 finished/idle（修复）；
  - task_notification 引发的 CLI 自启 turn → 遇到 `system/init` → midTurn（正确）；
  - **walk 走完只见到中性帧**：ring 未绕回 → 与今天一致（无语义帧 = 空 session）；ring 已绕回
    （§5.9 判据）→ 返回 `unknown`。前台 turn 的 tool_use 可能已被快照挤出，判 idle 会把正在跑的
    长 Bash 显示为 Ready；无条件判 midTurn 又会让本修复对长 workflow（ring 几乎必绕回）失效。
    `unknown` 由 session 层读 session JSONL 的尾窗裁决：末条主链记录是 `assistant` 且
    `stop_reason == "end_turn"`，或是 `result` → idle；否则 midTurn（既有 stray-result 恢复兜底）。读不到 JSONL 时按 midTurn。
    **尾窗是读窗口，不是文件尺寸上限**：`osutil.OpenRegular(path, 0)`（不设 maxBytes——真实 session JSONL 是 MB 级，v3 把 64KiB 写进尺寸上限，
    照抄会让每个文件都 `ErrTooLarge`、再按"读不到 → midTurn"处理，修复在长 workflow 这一主场景里失效）→ 由 Fstat 得 size →
    `ReadAt` 从 `max(0, size − 64KiB)` 起读 → 偏移不为 0 时丢掉第一段不完整的行 → 从后往前找最后一条完整的主链记录。窗口里一条完整记录都没有
    （末条记录本身超过 64KiB，例如很大的 tool_use input）→ 放大到 1MiB 重读一次，仍没有 → midTurn。
  - **裁决必须在 `startReadLoop` 之前完成**。v2 写的是 `SetCwdForLinker` 之后（`router_shim.go:404-406`），可那时
    `SpawnReconnect` 已经调过 `applyReconnectVerdict` 与 `startReadLoop`（`wrapper.go:625-641`）；代码明确要求 midTurn 状态、
    `reconnectedMidTurn` 与 adopted-turn latch 在 readLoop 启动前武装（`wrapper.go:629-635`、`adopted_turn.go:72-87`，#1778）。
    晚武装 midTurn：先到的 result 走非 CAS 路径被消费（`process_readloop.go:616`），之后再置 Running 就再也等不到 result，
    session 卡死在 Running；先武装 midTurn、之后降级为 idle：要在 readLoop 运行中撤销"永不清除"的 latch，还会与 CLI 自启 turn
    的 `system/init`（onSystemInit）竞速。所以改为：`SpawnReconnect` 增一个参数
    `resolveUnknown func(helloSessionID string) (idle bool)`，`reconnectVerdict` 得出 `unknown` 时在 `applyReconnectVerdict`
    **之前**同步调用它，把结果折成 idle / midTurn 再武装；参数为 nil（测试、无 workspace）时按 midTurn。session 层在调用
    `SpawnReconnect` 之前就有构造它所需的一切：`sess.Workspace()` 与 `state.SessionID`（`router_shim.go:365` 的
    `markTranscript` 正是用它们定位主 transcript，`managed_cost_end.go:35-55`；`backendProfile(...).ResumeTarget` 给出路径）；
    resolver 以 `handle.Hello.SessionID`（非空时）优先、否则 `state.SessionID`。resolver 是 `router_shim.go` 里构造的闭包，
    不新增 Router 方法（§5.6(6b)）。读尾窗如上（同步，在 `SpawnReconnect` 里、`shimReconnectTimeout` 内完成；这里没有 b.mu，也不在表事务里）。
  - 测试：replay 只剩中性帧且 ring 已绕回、JSONL 尾为 `end_turn` → Ready（**fixture 为 5MB 的 JSONL**，小 fixture 测不出尺寸上限的误用）；
    末条记录 200KiB → 放大窗口后正确裁决；为 tool_use → Running 且之后一个 live result 翻回 Ready；
    **live result 紧随 `startReadLoop` 到达**（fake shim 在 readLoop 启动后立即写出 result）时 session 仍到达 Ready（#1778 回归）。
  - 该修复同样惠及后台 Agent/Bash 任务。测试扩展 `process_reconnect_drain_bug_test.go:139`
    （TestIsMidTurn）、:218、`adopted_turn_test.go:37`。

## 6. Wire 协议

### 6.1 WS：新出站帧 `workflow_state`

```go
// internal/wsproto/wsproto.go 出站常量块（:25-45）
TypeWorkflowState MsgType = "workflow_state"

type WorkflowState struct {
    Type        MsgType           `json:"type"`
    Key         string            `json:"key"`
    Node        string            `json:"node,omitempty"`
    TaskID      string            `json:"task_id"`
    Epoch       string            `json:"epoch"`                  // board 版本空间（§5.8）
    Version     uint64            `json:"version"`                // board wire version
    BaseVersion uint64            `json:"base_version,omitempty"` // delta 的基线；full 时为 0
    Full        bool              `json:"full"`                   // full 帧只有 header + phases，永不带 agents
    ServerNow   int64             `json:"server_now"`             // ms；客户端算 elapsed 用（§7.4）
    RowsOmitted int               `json:"rows_omitted,omitempty"` // 本帧因尺寸预算未带的变化行数（wire-only）
    Workflow    workflow.WireView `json:"workflow"`
}
func NewWorkflowState(f WorkflowState) WorkflowState { f.Type = TypeWorkflowState; return f }

TypeWorkflowSet MsgType = "workflow_set"

type WorkflowSet struct { // 本订阅 board 当前的 task 集合；新增于 v3
    Type      MsgType  `json:"type"`
    Key       string   `json:"key"`
    Node      string   `json:"node,omitempty"`
    Epoch     string   `json:"epoch"`
    TaskIDs   []string `json:"task_ids"`   // Published 中全部 task（通常 ≤ 21；live 非终态 > 16 的病态情形 ≤ 37，§5.3）；空 board 为 []
    ServerNow int64    `json:"server_now"`
}
func NewWorkflowSet(f WorkflowSet) WorkflowSet { f.Type = TypeWorkflowSet; return f }
```

**为什么要 `workflow_set`**：`workflow_state` 是逐 task 的帧，协议里没有办法说"某个 task 已经不在了"。`/new` 或驱逐重建后，
推送 loop 换到新 board（新 epoch），而新 board 往往是空的、一帧都不发；store 按 sid 存（sid = key + node，`session_ident.js:11`，
`/new` 不改 key），旧 board 的条目原样留着，其中 running 的永远 running（旧进程的 interrupted 发布在旧 board 上，已无人订阅）。
naozhi 重启后不在 Ref 里的条目同理。`workflow_set` 在订阅开始时、每次换 board 时（**空 board 也发**）以及 Published 的 task 集合
变化时（board 裁剪淘汰、新增）发送，客户端据此删除条目。

`WireView` = `Workflow` 去掉 `json:"-"` 字段。所有模型生成的字符串在 Tracker 规范化时已
"redact → 截断"（§4.3），board 发布的是 immutable 对象，WireView 只做拷贝与行筛选，**不**再按订阅者
重复跑 `RedactSecrets`（其 `=` 分支每串约 17µs，2000 行 × 订阅者 × 帧不可接受）。该帧不含
`EventEntry`，不在 `wire_egress` 规则范围内（`rule_wire_egress.go:76-123`），所以 redaction 由
Tracker 负责，并配泄漏测试（仿 `wshub_eventpush_redact_paths_test.go`，覆盖每个字符串字段，含跨
截断边界的密钥）。

**版本语义**：版本只由 board 分配（§5.8），同一 `epoch` 内对每个 task 单调；行 `rev` = 该行内容
最后一次变化时的 version；HTTP（§6.2.1）与 Summary（§5.8）用同一版本空间。

**服务端（每个订阅一个 `workflowPushLoop`，每个 task 一个 `lastSent`）**：

- 每次唤醒先比较 Published 的 task 集合与本 loop 上次发出的 `workflow_set`：不同（含首次、换 board）→ 先发 `workflow_set`，
  再发各 task 的帧；从集合里消失的 task 删掉其 `lastSent`。
- `lastSent == 0`（首次 / 换 board），或该 task 的 `rows_gen` 与上次发送时不同（行集收缩，§5.8）→ `full:true`：header + phases，
  无 agents。
- 否则 → `full:false, base_version: lastSent`：header 与 phases 总是完整，`agents` 只含
  `rev > lastSent` 的行。
- **尺寸预算**：每帧序列化后 ≤ 192KiB。超出时按 failed → running → queued → stopped/skipped →
  done 的优先级装行，装不下的行数计入 `rows_omitted`。phases 至多 200 个——上限在 Tracker（§5.3），帧只是继承，
  HTTP 与 WS 看到的是同一组 phase（v3 只在帧上截断，HTTP 返回全部，两边对不上）。
- 入队成功（`trySendRaw` 返回 true，`wsclient.go:120-148`）→ `lastSent = version`（含
  `rows_omitted` 的帧也前进：客户端负责 HTTP 补齐）。失败 → `lastSent` **不前进**，250ms 后重试；
  下次 delta 仍以旧 `lastSent` 为基，自动包含遗漏的行。WS 在入队之后有序。
- **合并节奏**：结构变化或终态 → 下一次唤醒立即发；纯 progress → 每订阅每 task 至多 1 帧 / 秒。
- **背压**：两道深度门，**超过门限时不调用 `trySendRaw`**，只 arm 250ms 重试：
  - 非终态 workflow 帧：`len(c.send) > 8` → 跳过（latest-wins：积压的旧帧没有价值；v1 的 `> 192` 允许 ~192 个大帧排队、可达数十 MB）；
  - 终态帧与 `workflow_set`：`len(c.send) ≥ cap(c.send)/2`（生产 cap 256，`wshub_upgrade.go:76`）→ 跳过。v2 让终态帧总是尝试入队、
    失败每 250ms 重试，而 `trySendRaw` 每次满队失败都 `c.dropped.Add(1)`（`wsclient.go:120-148`），该计数按 client 累计、与所有发送方共享、
    永不清零，到 `wsDropThreshold=64`（`wsclient.go:60`，注释按"约 1 次 / 秒"设计）即断开；几个 workflow 同时结束、跨多个订阅 key 时
    每个 (订阅, task) 每秒 4 次，几秒内就把积压的 tab 踢掉——正是这条背压要避免的"强制断开、重连后又收全量"。
  深度门之下 `trySendRaw` 仍可能因竞争失败（计一次 drop），但不会形成按 250ms 累加的重试风暴。

**`workflowPushLoop` 生命周期**（新文件 `internal/server/wshub_workflow.go`）：

- 由 `completeSubscribe` 的**有进程分支**在启动 eventPushLoop 的同一处启动；`subs.install` 的 admit
  回调里改为 `clientWG.Add(2)`，`spawned` 守卫相应 `Done()` 两次。**不改** `eventPushLoop` 与
  `wshub_subscribers.go`（已 492/500 行）。
- 启动顺序：先 `board.Subscribe()`，再 `Load` Published，先发 `workflow_set`（空 board 也发），再发各 task 的初始 full 帧（自己发，用
  `trySendRaw`，失败按上面重试）——先订阅后快照，快照之后的变化必然再唤醒；`lastSent` 就在本
  goroutine 内，无交接问题。初始帧在 history 之后到达，客户端不依赖相对顺序。
- 唤醒源：board 通道、单个 `time.Timer`（到期取 min(限速窗口, 250ms 重试, 5s 存活检查)）、
  `c.done`、`h.ctx.Done()`。
- 每次唤醒与每次发送前：`gen, ok := h.subs.generation(c, key)`；`!ok || gen != myGen` → 退出
  （unsubscribe、`handleSubscribe` 接管、订阅过期都会走到这里；5s 存活检查保证无变化时也会退出）。
  这样 eventPushLoop 卡在 `resubscribeEvents` 的最长 60s 里，本 loop 照常推送（generation 在 swap 时
  不变）——进程退出时的 `interrupted`、结果文件终态都能送达。
- 每次唤醒：`cur := h.router.SessionFor(key)`；`cur != nil` 且 `cur.WorkflowBoard() != 当前 board`
  （`/new`、驱逐后重建）→ 退订旧 board、订阅新 board、清空 `lastSent` 与"上次发出的集合"——下一轮先发新 board 的
  `workflow_set`（可能是空列表），再发各 task 的 full。rename / respawn 以指针携带 board（§5.8），不会触发。
- 退出时退订 board；`defer` 停 timer。
- **无推送场景的兜底**：suspended 分支（`wshub_subscribe.go:99-128` 释放了订阅槽、不起 loop）、
  订阅过期（`resubscribeEvents` 超时后 `subscription_timeout`）与 WS 断开（5s HTTP 轮询，`session_list.js:846`）都没有 workflowPushLoop。
  此时面板靠 §5.8 的 sessions_update / 轮询：客户端在 `/api/sessions` 刷新后（`onSessionsApplied` hook，§7.6）比较当前 session 的
  `workflows[].{epoch,version}` 与 store，若 epoch 相同且 version 更大、或 epoch 不同，且该 task 近 5s
  没有 workflow_state 帧 → HTTP GET（§6.2.1），**按本地是否持有行选模式**：`!rowsLoaded` → `rows=none`（只要 header + phases；Summary
  不带 phases，所以仍需一次请求）；`rowsLoaded` → `since=<rowsAt>&epoch=&rows_gen=`（只要变化的行）。v3 一律拉全量：WS 断开时轮询几乎每次都看到
  version 前进，当前 session 的每个 running workflow 每 5s 整份重下一次（≤ 2000 行、~1MB 未压缩），折叠中的 workflow 也被置 `rowsLoaded=true`，
  与 §7.5"只在展开时拉行"矛盾。
- **suspended 订阅在进程出现时升级**：suspended 只在 session 变成 `running` 时才重订阅（`session_list.js:979-990` 的 case 3），
  而 shim 重接时 parent 通常是 `ready`，workflow 恰在 parent 空闲时最活跃——没有这一条，在 suspended 期间订阅的 tab 整个 run 都没有 loop。
  正常重启不受影响（`ReconnectShimsCtx` 在 `srv.Start` 之前同步完成，`cmd/naozhi/main.go:234,603`）；受影响的是运行中 shim 断联后 ≤ 30s 的
  重接窗口，以及启动时没接上、之后由 reconcile 接上的 shim。修在客户端（PR-12）：`session_list.js` 的 `sessions_update` 处理器在既有的
  "无订阅则自动订阅"恢复旁加一条——`sessionStream._subscriptionSuspended` 且 `subscribedKey === selection.key`，刷新后的快照 `protocol`
  非空（`managed_query.go:166-170` 只在有进程时填）→ `sessionStream.subscribe(...)`。重接本身推进 gen（`commitShimReattach` 的
  `MarkChanged`，`router_shim.go:635`）并发 sessions_update（`settleReconnected` → `notifyChange`，:518），所以 WS 连着时也能走到。
  这同时补上了事件流在同一窗口的缺口。服务端方案（无进程分支也起 workflowPushLoop）需要不带 unsub 的注册项或另一套 generation，不选。近 5s 没收到 `workflow_set` 时，store 里该 sid 不在 Summary 中的条目一律删除
  （Summary 恰好是面板要显示的集合：running 全部 + 最近 3 个终态），这是无推送场景下 `workflow_set` 的替代。
  `node` 非空且非 `local` 的 session 一律跳过：多节点模式下远端 session 会原样带着 `workflows` 合进 `/api/sessions`
  （`internal/dashboard/session/list.go:266-296`），而 §6.2.3 对远端恒返回 404，不跳过就会每次刷新都白打一次 HTTP。
  board 在无进程时的结构变化（R4 对账、interrupted）会立即触发 sessions_update，所以终态最迟在一次 sessions_update 后可见。

**客户端（`workflow_state.js` 叶子模块，§7.1）**，每个 `(sid, task_id)` 维护
`{epoch, version, rowsGen, rowsLoaded, rowsAt, rows, fetchInFlight, retryAt, buffered[]}`。v4 把两个版本分开：`version` 是 header / phases
已到达的版本，`rowsAt` 是本地行已完整的版本（只在 `rowsLoaded` 时有意义，恒 ≤ `version`）。full 帧只带 header，所以它能推进 `version`、
却推进不了 `rowsAt`；v3 只有一个 `version`，只好让 full 帧总丢行（每次重订阅都重拉），HTTP 响应又无条件写 `version = H`（可能倒退）。

以下"拉取"一律经 `needsFetch` 去重：`fetchInFlight` 时不另发，只记下"落地后还要再看一次"。模式：`full`（展开时 `ensureRows` 的首拉、
rows_gen / epoch 变化后）、`delta`（`since=rowsAt`）、`none`（`!rowsLoaded` 时的兜底刷新）。

| 收到 | 条件 | 动作 |
|---|---|---|
| `workflow_set` | — | 删除该 sid 下 `task_id` 不在列表中、或 `epoch ≠ 帧 epoch` 的全部条目；记录 sid 的当前 epoch |
| full | `epoch ≠ local.epoch` 或 `rows_gen ≠ local.rowsGen`（含新条目） | 替换 header/phases，`epoch`/`version`/`rowsGen` 取帧值；丢弃 rows、`rowsLoaded=false`；展开中 → 拉 full（在途则落地后按 epoch / rows_gen 不符丢弃再拉一次） |
| full | epoch 与 rows_gen 都相同 | `version` 取 max(local, 帧)，帧更新时替换 header/phases；**保留 rows**；`rowsLoaded` 且 `rowsAt < 帧 version` → 拉 delta；`!rowsLoaded` 且展开中 → 拉 full（在途则都不另发，落地后再看）。短暂 WS 抖动、内容未变的重订阅不再重拉 |
| delta | `epoch ≠ local.epoch` | 丢弃 → 拉 full |
| delta | `fetchInFlight` | 缓存进 `buffered`（含 header） |
| delta | `workflow.rows_gen ≠ local.rowsGen` | 丢弃本地 rows、`rowsLoaded=false`、`rowsGen` 取帧值，再按下面几行应用 header（服务端在 rows_gen 变化后本应发 full，这是防御） |
| delta | `version ≤ local.version` | 忽略 header；行同下一行规则（full 帧可能已把 header 推到前面，而行还停在 `rowsAt`） |
| delta | 其余 | header：`base_version > local.version` → 缺帧，拉（`rowsLoaded` ? delta : none）；否则应用 header、`version = 帧值`。行（仅 `rowsLoaded`）：`base_version ≤ rowsAt < 帧 version` → 按 index 整行替换合并、`rowsAt = 帧 version`；`base_version > rowsAt` → 拉 delta。`rows_omitted > 0` 且 `rowsLoaded` → 拉 delta 一次 |
| HTTP 响应（H，`rows_mode`） | 响应 epoch ≠ 当前 epoch（且已有 WS 帧） | 丢弃，重拉一次 |
| HTTP 响应 | 否则 | header：`H ≥ local.version` 才替换，`version = max(local.version, H)`——**永不倒退**。行：`full` 且（rows_gen 与本地相同，或 `H ≥ local.version` 时采用响应的 rows_gen）→ 整体替换、`rowsAt = H`、`rowsLoaded=true`；`full` 但响应较旧且 rows_gen 不同 → 丢弃行、拉 full；`delta` → 按 index 合并、`rowsAt = H`；`none` → 行不动。然后按序重放 `buffered` 中 `rows_gen` 相同的帧（按上面 delta 的规则，与 `version` / `rowsAt` 分别比较；rows_gen 不同的丢弃）。重放后 `rowsLoaded` 且 `rowsAt < version` → 拉 delta 一次 |
| HTTP 429 / 5xx / 网络错 | — | **保持当前状态**，`fetchInFlight=false`，`buffered` 保留；按指数退避（1s → 2s → … ≤ 30s，有 `Retry-After` 时取二者较大者）记 `retryAt`，到期且仍需要时重拉 |
| `/workflow` 的 HTTP 404 | — | 删除该条目（session 不存在、task 不在 board 里、或 remote node；§6.2.3 起 404 只表示这些）。`/workflow_agent` 的 404 不碰 store |

正确性：delta 携带 `rev > base` 的全部行（整行、最新值），header 与 phases 总是完整；**在同一个 `rows_gen` 内行不会被删除**
（board 发现某 index 消失就推进 rows_gen 并触发 full，§5.8），所以 `base ≤ rowsAt` 时它覆盖 `rowsAt` 之后变化的所有行；HTTP 的 `since`
响应同理（服务端只在 epoch 与 rows_gen 都相同时给 delta，否则给 full）。`AgentsCapped` 只是显示提示，永不触发重拉；只有 `rows_omitted`、
版本缺口、`rowsAt` 落后与 rows_gen 变化触发 HTTP。HTTP 走 per-IP 限流（§6.2.3），客户端对同一 task 的重拉做 1s 去抖。
**回到一个 session**：`renderWorkflowPanel` 重建 `<details>` 时 toggle 触发 `ensureRows`，重订阅的 full 帧也同时到达——两者经 `needsFetch`
合并，每个展开的 workflow 恰好一次请求（rows 已在切走时释放，§7.1，所以是一次 full）。

- **多 tab 合并**：可选，仿 `historyMarshalCache`（`wshub_eventpush_cache.go:39-140`），键
  `(key, task_id, epoch, version, base_version)`——现有指纹基于时间戳，不适合可变快照。
- Hub 通过 `*ManagedSession` 取 board（`WorkflowBoard()`），**不**给 `HubRouter` 加方法（14 个，
  godoc 说到 15 要重新设计）、**不**加 `HubOptions` 字段（`hubOptionsFieldBaseline = 16`，`tools/lint-server-handlers/rule_server_fields.go:33`）。
- reverse node 不转发这两种帧（NG3）。

### 6.2 HTTP

新包 `internal/dashboard/ext/workflows/{deps.go,routes.go,handler.go}`（`handle_decl` 规则要求
handler 不放 `internal/server`；dashboard 文件上限 800 行）。

#### 6.2.1 `GET /api/sessions/workflow?key=&node=&task_id=[&rows=none | &since=&epoch=&rows_gen=]`

返回 `{epoch, version, server_now, rows_mode, workflow: WireView(含 rows_gen), result?: {text, truncated}, logs?: [string],
logs_truncated?, result_unavailable?}`。`version` / `rows_gen` 与 WS 帧同一空间；`server_now`（ms）供客户端在没有 WS 帧时也能校准时钟（§7.4）。

- **行模式**（v4 新增；v3 只有全量，WS 断开时每个 running workflow 每 5s 整份重下，§6.1）：
  - 缺省 → `rows_mode:"full"`，`agents` 为全部行；
  - `rows=none` → `rows_mode:"none"`，`agents` 为空数组（header + phases + counts 照常）；
  - `since=V&epoch=E&rows_gen=G` → E 与 G 都等于当前值且 `V ≤ 当前 version` 时 `rows_mode:"delta"`，`agents` 只含 `rev > V` 的行；
    否则（epoch / rows_gen 已变、参数非法）服务端改回 `rows_mode:"full"`。`since` 与 `rows=none` 互斥（同时给 → 400）。
- `result/logs` 仅终态且存在 **taskId 匹配**的结果文件时提供；终态、RunID 已知但读不到（RunDir 尚未解析完成、文件缺失、不是 regular file、
  超限）→ 不带 result / logs，置 `result_unavailable:true`，**仍是 200**（v3 折叠为 404，客户端随之删除条目，§6.2.3）：

- 结果文件由 board 在 §5.6(4) 读取时解析**一次**，解析用只声明 `taskId, status, startTime, result,
  logs, totalTokens, totalToolCalls, durationMs, phases[].title, workflowProgress[]`
  的瘦结构体，其中 `workflowProgress` 的元素**直接复用 `clievent.WorkflowItem`**（与 stream 快照同形、同样不声明 preview；v3 只声明
  `{type,index,agentId,state}`，由它建出的行没有 label / model / tokens，R0 恢复的终态条目补行时就是一排空行）（**不**声明 `script` / `args`，**也不再声明 `resultPreview`**），裁剪后（`result` JSON 化后截 16KB；`logs` 每行 ≤ 500 runes、
  ≤ 200 行且合计 ≤ 64KiB，超出丢最旧并置 `logs_truncated`；均先 redact 后截断）缓存在 board 的终态 Workflow 旁
  （immutable，**≤ ~80KB / workflow**，随 board 的终态 LRU 淘汰，§5.3）；HTTP 直接从内存返回，不再逐请求开文件。
  v2 还缓存每个 agent 的 `resultPreview`（≤ 8000 runes × ≤ 2000 agents），所谓"≤ ~120KB"并不成立：本机 309-agent 的
  `wf_51427dfc-2fd.json`（575,652B）光 resultPreview 就有 116,790 字符，再加 16KB 的 result 已超；而 CC 自己把
  resultPreview 截到 ~400 字符（实测最长 401），8000 runes 的上限从不起作用，还会让同一 agent 的结果在终态后从 journal 的 ~8000 字
  缩成 ~400 字。per-agent 结果改为统一走 §6.2.2 的 journal 索引。
- 缓存缺失（例如重启后首次访问）→ handler 在请求 goroutine 上调用 board 的 `loadResult(task)`（与 R0 补行、sweeper 是同一个方法，按 task
  singleflight；占一个全局 I/O 槽位，取不到时不等待、本次返回 `result_unavailable`，客户端照常显示 header 与已有行）：锁外读一次（root 锚定的 `osutil.OpenRegularIn`，≤ 16MiB，§10），回到 b.mu 内复核 taskId 与条目代数后经
  `MergeResultFile` 合入**行、总计与结果缓存**（`!ResultLoaded` 时），按普通版本推进发布；本次响应返回合并后的状态。v3 只回填 result / logs，
  恢复出来的终态条目展开后仍是零行。

HTTP 已 gzip（`server.go:255`）。用途：展开 workflow 时拉行（full）、无推送兜底（none / delta）、版本缺口与 `rows_omitted`（delta）、
`rows_gen` 变化（full）、查看结果。

#### 6.2.2 `GET /api/sessions/workflow_agent?key=&node=&task_id=&index=`

per-agent 预览：`{index, agent_id, label, prompt?, result?, error?, transcript:bool}`。**只要 task 在 board 里、index 在行集里，就是 200**；
缺 transcript、缺 journal、RunDir 尚未解析完成或文件不是 regular file → `transcript:false`、不带 prompt / result（v3 把这些折叠为 404）。

- `prompt`：`agent-<id>.jsonl` 首行里的 prompt 文本（≤ 4000 runes），经 `subagent.ReadFirstPrompt`（§8.2，与
  `ReadFirstLineIDs` 同在 PR-10 落地）的有界读取。`ReadFirstPrompt` **一次解码**同时返回 `sessionId`、`agentId` 与 prompt，身份校验与 prompt
  共用这一遍（v3 每次请求各解一遍首行，每遍 `LimitReader(16MiB)`）；结果按 `(agentId, dev, ino)` 缓存在 board 里（LRU 64，§5.3；首行永不变化）。
- `result`：**running 与终态一律**取 journal 中该 agent 当前 agentId 的最后一条 `result` 行（≤ 8000 runes；实测最长 7974 字符），
  没有则不返回。journal 查找用 board 缓存的 **`agentId → 字节偏移`** 索引（journal 的 result 行带 agentId，§1.3）：
  - 按 `(task, dev, ino)` **singleflight** 建立——v3 在首个请求上同步扫描、无去重，per-IP 突发 20 个请求即可并发起 20 遍 64MiB 扫描；
    建立在 I/O 派发的槽位上执行，请求 goroutine 等待同一次扫描的结果；
  - 之后按文件尺寸增量续扫，记下已扫到的 `(dev, ino, size)`；**inode 变化或文件变小**（被替换 / 截断）即丢弃索引重建，不再给出过期偏移；
  - 单行上限 `limits.MaxStreamJSONLine`（16MiB，`internal/limits/limits.go:20`；与 stream 解码的 10MiB 不同），journal 文件 ≤ 64MiB
    （超过则不建索引、不返回 result），经 root 锚定的 `osutil.OpenRegularIn` 打开（§10）。索引 ≤ ~80KB，随条目淘汰（§5.3）。
- queued agent（从未有 agentId）：**200**，只返回 `index` / `label`、`transcript:false`（v3 的 §6.2.2 写"只返回 label"、§11.2 却写 404，现统一为前者）。
  所有字符串先 redact 后截断。

#### 6.2.3 校验、鉴权、限流、降级

- 鉴权：挂在 `mountRoutes → apiChain = RequireSameOrigin + RequireAuth`（`routes.go:327-348`），
  无豁免。`TestAPIUnauthenticatedRejected`（`auth_coverage_test.go:30-60`）自动覆盖（只用 query
  参数，无新 path wildcard）。
- **校验顺序**（两个 handler 相同，仿 `agentevents/handler.go:81-84`）：
  1. `session.ValidateSessionKey(key)` 失败 → 400（与 agentevents 一致）；
  2. `node` 非空且非 `local` → 404（同 `agentevents/handler.go:121-140`）；
  3. `task_id` 过 `^[a-z0-9]{1,32}$`（同 `agentevents/handler.go:35`）、`index` 为 1..2000 的整数、`rows` / `since` / `epoch` / `rows_gen`
     形态合法且不互斥冲突，否则 400；
  4. `router.SessionFor(key) == nil`、该 session 的 board 里没有此 task，或（`/workflow_agent`）该 index 不在行集里 → 404。
- **限流**：`apiChain` 本身不限流（`sendLimiter` 只挂在 send/bind/upload，`build_dashboard.go:36`、
  `dashboard_send.go:79,349`）。仿 `ext/memory`（`handler.go:41,68-69,144`）注入 `IPLimiter` 依赖，两个
  端点共用一个 per-IP 令牌桶（10 rps / burst 20，与 memory 同参），超限 429。没配 TrustedProxy 的反向代理后面，所有 tab 共用一个 IP 的桶；
  客户端遇 429 的处理见 §6.1 客户端表（保持状态、退避重试）。
- **404 只表示"资源不存在"**（session、task、index、remote node，即上面的第 2、4 步）。校验之后的磁盘失败（RunDir 未解析、路径越界、
  文件缺失、不是 regular file、超限）**不是** 404：`/workflow` 返回 200 不带 result / logs 并置 `result_unavailable:true`，`/workflow_agent`
  返回 200 置 `transcript:false`、不带 prompt / result。v3 把它们一律折叠为 404，而客户端遇 404 会删除条目（§6.1）——一个可选的磁盘产物
  （CLI 中途死亡时没有结果文件，§1.3 的 21 个 run 中 2 个；或被换成 FIFO）就能删掉 board 上仍存在的 workflow，之后的 delta 又因 epoch / 条目缺失
  触发重同步、再 404，条目反复闪现。响应从不包含路径；"文件是否可读"与 transcript 同属已认证可见的信息，不构成新的泄露。

### 6.3 注册清单（一次做齐，否则 CI 必红）

| 产物 | 动作 | 闸门 | PR |
|---|---|---|---|
| `SessionSnapshot.Workflows` | 重生成 `internal/dashboard/session/testdata/rest.schema.json`（`-update-rest-schema`） | `TestRESTSchema_IsGenerated` | PR-8 |
| `sessions_shape_test.go` `allowed` | 加 `workflows` | 该测试 | PR-8 |
| `wsproto.go` 常量 + struct + `New*`（`workflow_state`、`workflow_set` 两种帧） | 新增 | `literal_ban_test.go:21`（只能经 `New*` 构造） | PR-11 |
| `wsproto/registry.go` Frames 示例（每个字段非零） | 新增 | `TestSchema_CoversEveryFrame`、`TestFramesRegistry_TypeStamped` | PR-11 |
| `wsproto.schema.json` | `go generate ./internal/wsproto` | `TestSchema_IsGenerated`（`schema_contract_test.go:86-91`） | PR-11 |
| `static/contract.js` | `go run ./tools/gen-contract`；PR-10 的两条 `/api` 路由让 API 表多 2 行（`contractjs.go:57-66`），PR-11 再加 WS 常量与 ENUMS——两者都要手改 js-ratchet 基线（`contract.js.lines`，现 110）并追加 `js-ratchet:TOTAL.lines` 台账行（§14） | `TestContractJS_Current`（只查新鲜度）、js-ratchet `--check`、`ratchet-raises` | PR-10/11 |
| contractjs ENUMS `WORKFLOW_STATUS` / `WORKFLOW_AGENT_STATE` | 新增；`check-enum-literals.mjs` 扩展两条：① `workflow_state.js` 的状态显示表（键不加引号）须**恰好**含这两个枚举的键（复用 `tableKeys`，加文件参数；仿 `DEATH_REASONS`）；② **只在 `workflow_state.js` 与 `workflow_view.js` 内**禁止这些值的整串字面量（`literalHits` 限定这两个文件）。**不做全仓禁令**：running / failed / completed / queued / done / unknown / stopped / skipped / paused 在现有十几个 static 文件里作为 session / cron / agent 状态被比较（如 `dashboard.js:335` `sd.state === 'running'`、`agent_view.js:67` `a.status === 'completed'`、`event_stream.js:807` `msg.status === 'queued'`），`literalHits` 分不清比较的是哪个字段，全仓禁令首跑即红——`SESSION_STATE` 不纳管也是这个原因（`check-enum-literals.mjs:14-18`、`contractjs.go:68-71`，归 #2909 的后续）。夹具进 `scripts/check-enum-literals.test.mjs` | `node scripts/check-enum-literals.mjs` + 其 test | PR-11 |
| 前端 `wsm.on(NZ_CONTRACT.WS.workflow_state, …)` 与 `wsm.on(NZ_CONTRACT.WS.workflow_set, …)` 各一个 | `workflow_view.js` 模块顶层，参数名 `msg`。**handler 不得把 `msg` 交给 import 进来的函数**：check-ws-receivers R6 会拒（`check-ws-receivers.mjs:14-19,127-128`，夹具 `check-ws-receivers.test.mjs:114-117`）。R6 允许两种形态：在 handler 里逐字段读 `msg.<field>` 拼成普通对象再交给叶子模块，或转交给参数名为 `msg` 的**同文件**函数（R6 有意放行，因为那个函数里的 `msg.<field>` 仍在扫描范围内；`cron_view.js:2020-2021` 的 `cronApplyRunStarted(cronMsgOf(msg))` 就是这种，v3 把它当作逐字段读的先例、又把这种形态称为"钻空子"，两处都不对）。本 RFC 用前者，字段一目了然：`(msg) => applyFrame(store, { key: msg.key, node: msg.node, task_id: msg.task_id, epoch: msg.epoch, version: msg.version, base_version: msg.base_version, full: msg.full, server_now: msg.server_now, rows_omitted: msg.rows_omitted, workflow: msg.workflow })`。`check-ws-contract` 扫全部 static/*.js 的顶层 `msg.<field>`（`check-ws-contract.mjs:83-97`），只覆盖到 `msg.workflow` 这一层；嵌套读取见下一行 | `check-ws-contract.mjs:34-45`、`check-ws-receivers.mjs` R1-R7（新增 R6 正反夹具：转交 `msg` 被拒、逐字段对象通过） | PR-11 |
| `workflow_state.js` 读取嵌套字段的函数（`applyFrame` / `applyHttp` / `reconcileSummaries` 及其内部 helper） | 入参用 JSDoc 标注 def 短名：WS 帧里的 `workflow` → `/** @param {WireView} w */`、行 → `{Agent}`、phase → `{Phase}`，HTTP 响应 → `ext/workflows` REST schema 里的响应 def，Summary → `{Summary}`（短名与既有 def 冲突时在 Go 侧换不冲突的类型名）。check-ws-contract 的嵌套检查只沿"从 `msg` 出发的成员链"和"带 def 短名 JSDoc 类型的参数"两条路跟踪（`scripts/ws-contract-nested.mjs` 文件头）；v3 把 `msg.workflow` 原样交给叶子模块、参数不带类型，`rows_gen`、`agents[].rev`、`agents_capped`、`prev_agent_ids`、`phases[].counts` 等读取都不受检查，Go 侧改一个 json tag 浏览器里就静默失效。本表下文登记的 workflow REST schema 也只经这条路生效 | `check-ws-contract`（嵌套部分；`typedFns` 计数应增加） | PR-11 |
| `handlerSet` 字段、`build_server.go` 构造（注入 IPLimiter）、`registerDashboard` mount | 新增 | `routes_snapshot_test.go:195-212,406-427` | PR-10 |
| `testdata/routes.golden.json` | `UPDATE_GOLDEN=1 go test -run TestRoutesSnapshot ./internal/server/` | `TestRoutesSnapshot` | PR-10、PR-11（静态资源路由） |
| workflow REST 响应类型 | `internal/dashboard/ext/workflows/rest_schema_test.go` + `testdata/rest.schema.json`（仿 session 包的 `restResponses` 生成器）；路径追加到 `test/e2e/check-ws-contract.mjs:120` `REST_SCHEMAS` 与 `scripts/check-mock-rest.test.mjs:13` 的 schema 读取 | 新包自己的 `TestRESTSchema_IsGenerated`、`check-ws-contract` | PR-10 |
| mock-server 两个 workflow 路由 | 新增，含 `/workflow` 的三种行模式（`rows=none`、`since=` → delta / 退回 full）与 `result_unavailable`；**不**扩 `check-mock-rest.test.mjs:51-55` 的 `ROUTES`（那是 EventEntry 夹具专用），另写 workflow 响应的 schema 校验用例 | `check-mock-rest` | PR-10 |
| 新 ES module 资产 | `static_assets.go` `//go:embed` + 资产表行、`routes.go` 静态路由 + golden | `TestStaticJS_ModuleInventory`（`static_module_inventory_test.go:13-21`）、`TestDashboardPage_ImportMapAndPreload`（`static_versioning_test.go:78`） | PR-11 |
| lint-server-handlers | `-mode fail`；新 server 文件 ≤ 500 行，**不抬基线** | CI | PR-10/11 |

`scripts/js-deps-freeze.mjs` 的 LOAD_ORDER 与 `js-deps-baseline.json` 只覆盖 7 个历史
classic→module 文件（`:36-44`），新 ES module **不**登记在那里。

## 7. 前端设计

### 7.1 模块与状态

- 新叶子模块 `internal/server/static/workflow_state.js`（**不** import `wsm` / dashboard.js）：
  store 与 §6.1 客户端状态机的纯函数——`applyFrame(store, frame)`、`applySet(store, set)`、`applyHttp(store, sid, resp)`、
  `applyHttpError(store, sid, taskId, status, retryAfter, now)`、`needsFetch(entry, now)`、`reconcileSummaries(store, sid, summaries)`、
  `releaseRows(store, sid)`、`visibleWorkflows(store, sid)`、`announceable(store, sid, selectedSid)`（§7.7）。入参 `frame` / `set` 是
  `workflow_view.js` 的 handler 逐字段拼出的普通对象，不是 `msg` 本身（§6.3 R6）；其中的 `workflow` / 行 / phase / HTTP 响应 / Summary
  参数一律带 def 短名的 JSDoc 类型（§6.3），嵌套字段因此受 check-ws-contract 校验。配
  `scripts/workflow-state.test.mjs`（`node --test`，CI 并入 `ci.yml:396-403` 那组，先例
  `cron_state.js` / `session_stream.js`）。
- 新 ES module `internal/server/static/workflow_view.js`：薄层。
  - 模块内 `const store = new Map()`（sid → Map(taskId → entry)），**不**
    `export let`（`nz/no-exported-let`）；跨 turn 存活，不放进 `turnState`（turn 边界会清，
    `running_banner.js:306-326`）。每 sid 的条目集合由 `workflow_set`（或无推送时的 Summary）裁剪，另有 21 条的 LRU 兜底；
    **行只为当前 session 保留**：切走 session 时 `releaseRows(prevSid)` 丢掉各条目的 rows、置 `rowsLoaded=false`（header / phases 保留），
    回来时展开的 workflow 经 gzip HTTP 重拉。切走后旧 session 的行本来就不再收 delta、回来时的 full 帧也会置 `rowsLoaded=false`，留着只是占内存
    （每个 workflow 最多 2000 行，移动端尤甚）。
  - 顶层只有 `wsm.on(...)` 注册（`nz/no-module-side-effects` 豁免项，`eslint-plugin-nz.mjs:26-42`）；
    其余全部是导出函数：`renderWorkflowPanel()`、`onSessionsRefreshed()`（§6.1 兜底；从 `state.js` 的
    `sessionList.lastSidebarData` 读本次 payload，`session_list.js` 在调 hooks 之前已写入）、
    `onWorkflowSessionSwitched(prevSid)`、`dropWorkflowSession(sid)`、`workflowActions`（handler 表，由 `dashboard.js:2808` 的
    `registerActions` 并入——新模块不能顶层调用 `registerActions`，同理也不能顶层调 `onSessionsApplied`：由 `dashboard.js` 在
    :346-347 那组旁加一行 `onSessionsApplied(onSessionsRefreshed)`）。
  - 不 import dashboard.js（D4-1，js-ratchet 的 `importCycles` 必须为 0）。
- 新文件登记：`static_assets.go` 的 `//go:embed` + 资产表行、`routes.go` 静态路由 + golden、js-ratchet
  baseline 行、CLAUDE.md:242 模块段落。闸门是 `TestStaticJS_ModuleInventory` 与
  `TestDashboardPage_ImportMapAndPreload`（§6.3）。**由谁 import**：`dashboard.js`（v3 写"dashboard.html 或 event_stream.js"，而 §7.6
  又说 event_stream.js 不碰，PR-11 的文件清单里两者都没有——模块从不被执行，`wsm.on` handler 从不注册；import map 与 modulepreload
  只取回模块、不执行它）。PR-11 在 `dashboard.js` 加一行副作用 import `import './workflow_view.js';`，handler 从 PR-11 起生效、store 开始填充
  （尚不渲染）；PR-12 把它换成具名 import（`renderWorkflowPanel`、`onSessionsRefreshed`、`onWorkflowSessionSwitched`、`dropWorkflowSession`、
  `workflowActions`）。
- **js-ratchet**：TOTAL.lines 只能降（`scripts/js-ratchet.mjs`），`ratchet-raises` 是必过 job
  （`ci.yml:60-69,80-100`），**只认本 PR 追加**的 `scripts/ratchet-raises.jsonl` 行，from/to 须精确
  （`tools/ratchet-raises/ledger.go`）。所以**任何**改动 `static/*.js`（含生成的 `contract.js`）或 golden pin 的 PR——PR-10 至 PR-15——
  都要追加自己的台账行（§14 逐 PR 列出预期 gate）；v3 只算了 PR-11 至 PR-15，漏了重生成 contract.js 的 PR-10。台账键要按
  `tools/ratchet-raises/metrics.go:244-287` 取：`lines` / `configureDeps` / `deadInjections` / `innerHTMLAssign` / `htmlInsert` / `lateBindings`
  只按总和计（`js-ratchet:TOTAL.<name>`），per-file 的 `lines` **不是**台账键，只需手改 js-ratchet 基线；`maxFnLines` / `fnOver100` 才有 per-file 键
  （新文件的键是新键，由 `MAX.maxFnLines` / `TOTAL.fnOver100` 兜住）。pin 只有 `test/e2e/golden/pins.json` 列出的文件（`golden:<file>`），
  `internal/server/testdata/routes.golden.json` 不是 pin。**PR-10 之前**开一个打了 `ratchet-raise-approved`
  标签、覆盖整个特性的 issue，各 PR 的台账行都引用它（一个 issue 可被多行引用，先例 #3120）。
  innerHTML / htmlSinks（_global 175）**不增**：全部用 DOM API + `textContent` 构建。

### 7.2 挂载位置

`renderMainShell`（`dashboard.js:1198-1213`）在 `#session-runs-panel` 之前加一行
`<div class="workflow-panel nz-hidden" id="workflow-panel"></div>`，末尾补一行
`renderWorkflowPanel()`。dashboard.js 现 2860 行、硬上限 2878（`scripts/js-ratchet.caps.json`），余量 18 行。本特性对它的**全部**净增
（v3 只算了上面两行）：PR-11 的副作用 import +1；PR-12 的上面两行 +2、import 改具名（多行 import 块）约 +1-2、`registerActions` 表项 +1、
`onSessionsApplied(onSessionsRefreshed)` +1、会话切换处的 `onWorkflowSessionSwitched` +1，合计约 +7，在余量之内；超出时把 import 并进既有的
import 块。

- **不放进 `#running-banner`**：banner 在 parent 空闲时隐藏（`running_banner.js:207-212`），移动端
  键盘弹出时 `display:none!important`（`responsive.css:182`），且每个事件整块 innerHTML 重建
  `#rb-agents`（`running_banner.js:179`）。
- 面板位于 transcript 上方，样式同 session-runs-panel 的安静 chrome（`css/cron.css:285-286`：
  `border-bottom: 1px solid var(--nz-border); background: var(--nz-bg-1)`），无阴影。
- **整个 `#workflow-panel` 限高**：桌面 `max-height: 42vh`、≤768px `32vh`，`overflow: auto`。
  v1 只限单个 workflow 的展开体，2-3 个 running 同时展开就会把 `#events-scroll`（drill-in transcript
  也渲染在这里，`agent_view.js:199,297,348`）挤出视口。
- 每个 workflow 一个 `<details>`：桌面**只自动展开最新启动的那个 running**，其余 running 与终态默认
  折叠；最多显示 running 全部 + 最近 3 个终态（与 Summary 的集合相同；更早的不显示，Q11）。用户手动展开状态记于
  sessionStorage（Q10）。展开时若 `!rowsLoaded` 则 HTTP 拉行（§6.1）。
- **展开检测**：`toggle` 事件不冒泡，也不在 `nz_util.js` 的委托事件表里（`nz_util.js:383-394`）；`session_header.js:148-153` 用的
  document 级捕获监听只因它在 `sideEffectLegacy` 名单里才被允许，新模块受 `nz/no-module-side-effects` 约束不能照抄
  （`eslint-plugin-nz.mjs:26-42`）；只给 `<summary>` 挂 data-action 点击又覆盖不了代码设置 `open` 的情况（含上面的自动展开）。所以
  `renderWorkflowPanel` 在**创建**每个 `<details>` 时给它挂 `toggle` 监听（不是模块顶层副作用），监听里调用唯一的
  `ensureRows(sid, taskId)`：`open && !rowsLoaded` 即 HTTP 拉行。浏览器对代码设置 `open` 同样派发 `toggle`，所以自动展开也走这条路。
  区分用户与代码：自动展开前先置 `details.dataset.autoOpen = '1'`，监听里见到该标记就清掉、**不**写 sessionStorage；只有用户触发的
  展开 / 折叠才持久化（同 `session_header.js` 的 `data-user-toggled` 思路）。e2e 覆盖：点击展开拉一次行；自动展开也拉一次行；
  自动展开不写 sessionStorage。
- banner 仅补 `toolVerbs.Workflow = '启动 Workflow'`（`running_banner.js:111`）。

### 7.3 布局

```
▾ ● probe · tiny probe          Sum 2/2   ✓2 ▶1 ⏳0 ✗0 /3   53.2k tok · 0 tools · 12.7s
  Ask ▰▰▰▰▰▰▰▰▰▰ 2/2
    ✓ A   opus-5-5[1m]   17.7k tok · 0 tools · 2.2s
    ✓ B   opus-5-5[1m]   17.7k tok · 0 tools · 5.9s
  Sum ▰▰▰▰▰▱▱▱▱▱ 0/1
    ▶ C   opus-5-5[1m]   Read · src/foo.go          ×2(attempt)
  [完成后] 结果  {"r":["4","Paris"],"s":"ok"}   日志(1)  ▸
```

- phase 进度条用原生 `<progress max value>`——无需 inline style，满足 CSP（`dashboard_csp.go:82-97`、
  `static_inline_style_test.go:24`）。
- 行内顺序：failed → running → queued → stopped/skipped → done；done 的 phase 默认折叠。
- agent 行在 `views.css` 里写**独立的 `.wf-*` 规则**（`.wf-row`、`.wf-name`、`.wf-detail`、`.wf-stat`……），视觉上对齐 banner 的 agent 行。
  v3 说"可复用 `.sa-*` 类"不成立：`split_view.css:91-99` 的每条 `.sa-*` 规则都限定在 `.rb-agent-row` 之下（`.rb-agent-row .sa-name` 等），
  放在 `.wf-row` 上不产生任何样式。**不用 `.rb-agent-row`、不设 `data-task`**：
  `agent_view.js:638-647` 在 document 上委托了 `.rb-agent-row[data-task]` 的点击，调用不带第二参的 `switchTo(taskID)`。workflow 行若沿用这对
  类名 / 属性，一次点击会触发两次 `switchTo`（`switchSeq` 前进、取消第一次 fetch），后执行的那次还会丢掉 crumb。workflow 行用 `wf-row` /
  `wf-*` 类，agentId 放在 `data-agent-id`。新类加到 `views.css`
  （避免新 CSS 文件带来的 `dashboardCSSFiles` 登记，`static_events_panel_contract_test.go:206`）。
- **Token**（各状态必须可区分）：**字形 + `.sr-only` 文本**是每个状态的区分手段；颜色只额外区分 running / done / failed 三种。
  stopped、skipped、queued 是中性状态，颜色有意相近，不声称"颜色双编码"——v3 这么写，但 stopped 与 skipped 同为 `--nz-text-mute`，浅色主题下
  queued 的 `--nz-text-dim`（`#6e7781`）与 `--nz-text-mute`（`#656d76`）也几乎相同（`tokens.css:254,258`）。为三者再找三种强调色只会与
  running / 告警色冲突，收益不大：

  | 状态 | 字形 | 颜色 token |
  |---|---|---|
  | running | ▶ | `--nz-status-running`（与 sidebar 运行点一致，amber） |
  | done | ✓ | `--nz-ok` |
  | failed | ✗ | `--nz-err` |
  | stopped | ■ | `--nz-text-mute`（v1 用 `--nz-warn`，与 running 同为 `var(--nz-amber)`，`tokens.css:37,231`） |
  | skipped | ⤼ | `--nz-text-mute` |
  | queued | ⏳ | 行文本 `--nz-text-dim`（`--nz-text-faint` 在浅色主题为 `#afb8c1`，对白底约 2:1，只用于装饰） |

  字号 `--nz-fs-sm` / `--nz-fs-xs`；间距 `--nz-space-*`；选中态（drill-in 中的那一行）用
  `--nz-selected-bg` / `--nz-selected-bar`；不出现裸 `#hex` / `px` 字号。此纪律靠 review 维持——
  `static_style_ratchet_test.go` 与 `static_light_theme_parity_test.go` 已在 04934631 删除，现存的
  `static_inline_style_test.go` 只管 `style=` 属性；PR-12 截图覆盖 light/dark。
- 可点击行：行内主体是真 `<button type="button" class="wf-row-btn" data-action="wf-open-agent">`
  （原生 Enter / Space 激活；`dashActivate` 只认 Enter，`dashboard.js:2802-2807`，不改它以免波及全局）；
  queued 且从未有 agentId 的行渲染为非按钮。
- `degraded` 非空时 header 末尾加一个 chip（文本取显示表：`snapshot_stale` → "连接中断，进度可能过时"、`no_snapshot` → "暂无明细"、
  `decode_error` → "部分字段无法解析"、`snapshot_dropped` → "明细过大，已停止更新"、`too_many` → "仅显示概要"），`--nz-text-mute`，不改状态字形。
  status 为 `unknown` 且 `raw_status == "unclaimed"`（R5）时状态文本为"状态未知（进程未认领）"。

### 7.4 移动端与时间

- ≤768px：`<details>` 一律默认折叠成一行摘要；面板整体 `max-height: 32vh; overflow: auto`。
- `body.kbd-open` 时**整个 `#workflow-panel` 隐藏**（`body.kbd-open #workflow-panel{display:none!important}`，与
  `responsive.css:182` 对 banner 的处理一致）。v3 只隐藏 `.wf-body`、保留每个一行摘要（最多 16 running + 3 终态），而 32vh 按完整视口计，
  键盘弹出后可见区域缩小（`.main` 跟随 visualViewport，`mobile_nav.js:62-76`），面板会占掉剩余区域的大半——banner 那条规则正是为此。
- 行内只显示 label + 状态 + tokens，last tool 换到第二行并单行省略。
- **elapsed 用服务端时钟**：`workflow_state` / `workflow_set` 帧与 §6.2.1 的 HTTP 响应都带 `server_now`；客户端每次收到都记
  `offset = server_now − 到达时的 Date.now()`（模块级一份，全局共用），elapsed = `max(0, (Date.now() + offset) − started_at)`；
  终态用 `duration_ms`。避免手机 / 隧道访问时浏览器时钟偏差导致负值。**还不知道 offset 时**（例如页面在 suspended / WS 断开时加载，
  只有 `/api/sessions` 的 Summary，它不带服务端时间）不显示 elapsed；`started_at == 0`（来源未知，§4.2）同样不显示。一个共享 1s interval，仅当可见区域内存在 running workflow 时
  启用；不复用 turn timer。
- 截图验证 iPhone 13（mobile-safari CI 非必过，`ci.yml:288-325`，但截图必看）。

### 7.5 400-agent 节流

- 帧到达即写 store（O(changed rows)），渲染走 `requestAnimationFrame` 合并 + 最小 250ms 间隔。
- DOM 按 `index` 建 keyed Map（index → row element），只对 `rev` 变化的行改 `textContent`；
  phase header 只改计数与 `<progress>.value`。
- 每 phase 最多渲染 60 行，"显示其余 N 个"按钮（data-action）在客户端分页展开；折叠的 phase / workflow
  不渲染行。
- 行只经 gzip HTTP 首次加载（展开时）；WS 只带 header 与变化行，每帧 ≤ 192KiB（§6.1）。

### 7.6 与既有前端路径的关系

- `event_stream.js` 的 `onEvent`（:762-794）不新增 workflow 逻辑（专用帧已覆盖），因此不碰
  "第二个无条件 `wsm.on(WS.event)` 会抛"（`ws_manager.js:107-113`）的限制。
- task_type 被 `ForWire` 剥离（`clievent/wire.go:47-55`），前端本就无法从事件流识别 workflow——专用帧
  是唯一判别来源。
- 会话切换：store 按 sid 保留 header / phases，切走时 `onWorkflowSessionSwitched(prevSid)` 释放旧 sid 的行（§7.1）；session 删除时
  `dropWorkflowSession`；订阅后的 `workflow_set` 裁掉不在新 board 里的条目（`/new` 后面板清空），随后的 full 帧覆盖 header。
- `/api/sessions` 刷新（`sessions_update` → `debouncedFetchSessions`，`session_list.js:1072`）后经 **`onSessionsApplied` hook**
  调用 `onSessionsRefreshed`（§6.1 的无推送兜底）。挂在这个 hook 上是有意的：它只在 `sessionsUnchanged` 判定 payload 确有变化之后执行
  （`session_list.js:275-286`），而计数型 sessions_update 经 `BumpVersion` 推进了 `stats.version`（§5.8），所以 WS 连着时也会走到。
  注册由 `dashboard.js` 完成（§7.1），这条 hook 不改 `session_list.js`（下面 suspended 升级那一条改它）。node 非 local 的 session 跳过（§6.1）。
- WS 断开时 HTTP poll 回退（`event_stream.js:23,439`）的 `/api/sessions` 轮询同样经
  `onSessionsRefreshed` 追平（断开时 `sessionsUnchanged` 不短路），按 §6.1 的行模式只取 header 或增量行；WS 恢复、resubscribe 的
  `workflow_set` 与初始 full 帧再接管（epoch 与 rows_gen 未变时保留行，§6.1）。
- 在 session 没有进程时订阅的 tab（suspended），在之后的 sessions_update 里看到该 session 有了进程（快照 `protocol` 非空）即重订阅
  （§6.1，PR-12 改 `session_list.js` 的 `sessions_update` 处理器，几行）；不再只等 `running`。

### 7.7 无障碍

沿用 `test/e2e/a11y_live_regions.test.js:1-18` 钉住的约定（流式 UI 不是 live region；跳动的计时
`aria-hidden`；边沿事件经 `#sr-announce` 播报一次）：

- `#workflow-panel` 不设 `aria-live`；elapsed / tokens 计数 span `aria-hidden="true"`，其可访问文本在
  `<summary>` 的 `aria-label` 里只含静态信息（名称、状态、done/total）。
- workflow 进入终态时经 `#sr-announce` 播报一次："Workflow <name> 已完成 / 失败 / 已终止（done/total）"；
  **只播报当前打开的 session**：帧的 sid（key + node）等于当前选中 sid 才播报。workflow 帧会到达每个已订阅的 key——包括 `cron_live.js:37-49`
  自己发起的 `cron:<jobId>` 订阅，以及切走时退订尚未生效前在途的帧——store 也保留非当前 sid 的 header（§7.1）；不加限制就会念出用户没在看的
  session 里的 workflow 结束，违反 `a11y_live_regions.test.js:1-18` 的规则 (d)（切走 / 没在看的 session 保持安静）。非当前 sid 的终态同样记入
  "已播报"集合，所以之后切到那个 session 也保持安静（它的终态已是"初次看到即终态"）。
  页面初次加载已是终态的不播报；每个 `(task_id, 终态 status)` 在页面生命周期内至多播报一次（R4 / 进程结束合成的 interrupted
  若被重接翻回 running、之后又以同一终态结束，不重复播报，§5.6）。`snapshot_stale` 与 R5 的 `unknown` 不是终态，不播报。
- 每个状态字形包 `<span aria-hidden="true">✓</span><span class="sr-only">已完成</span>`（文本取
  状态显示表）；屏幕阅读器不读字形。视觉隐藏类用仓里已有的 `.sr-only`（`css/split_view.css:145-148`，`dashboard.html:213` 的
  `#sr-announce` 也用它）；v2 写的 `nz-sr-only` 不存在，会让"已完成 / 失败"作为可见文字出现在每个字形旁。
- `<progress aria-label="<phase> 2/5">`。
- 可点击行是真 `<button>`（§7.3），Enter / Space 原生可用；不可点的 queued 行不进 tab 序。
- e2e：在新 spec 中断言面板无 `aria-live`、终态恰好一次 `#sr-announce` 写入、按钮可 Space 激活、状态文本标签被视觉隐藏
  （`boundingBox` 为 1×1）；**非当前 session**（另一个已订阅 key，如 cron live）的 workflow 终态不写 `#sr-announce`；之后切到该 session
  仍不写。

## 8. Drill-in 设计

### 8.1 路径解析

**唯一的根**：`r.hist.claudeDir` 并非可配置——`cmd/naozhi/main.go:165-167` 只把它设为
`$HOME/.claude`，经 `RouterConfig.ClaudeDir`（:221）传入；`claudefs.DefaultDir()`、server 的 `resolveClaudeDir()`
（`build_server.go:66`）与 `agentevents` 的 `claudeProjectsAllowedRoot`（`handler.go:291-300`）得出的是同一值。v1 的"两个
claudeDir 来源"不成立。但 v2 说"由 router 注入的 claudeDir 求出一次、传给 agentevents.New"并不符合现状：`agentevents.New`
今天自己调 `os.UserHomeDir()`，`Deps` 里没有 root，`ServerOptions` 也没有 ClaudeDir。v3 的做法是**一个函数、一个输入**：

- 新导出 `claudefs.ResolvedProjectsRoot(claudeDir string) string`：`ProjectsRoot(claudeDir)` 后 `EvalSymlinks`，失败（首次运行）回落词法路径——
  即把 `agentevents.claudeProjectsAllowedRoot` 的逻辑搬进 claudefs。
- **server 侧**在构造时由 `resolveClaudeDir()` 的结果调用一次，传给 `agentevents.Deps.ProjectsRoot`（新字段；为空时回落旧逻辑，
  测试不受影响）、workflows handler 的 Deps，以及 `HubOptions.AllowedRoot`（复用既有槽位，见 §8.3、PR-4）。
- **session 侧**在 `NewRouter` 里由 `RouterConfig.ClaudeDir` 调用一次，存进 `HistoryIO.projectsRoot`（与 `claudeDir` 同处，
  `history_io.go:24-27`；**不**加 Router 字段，`routerFieldBaseline = 18`）。board 的路径解析、§5.6 的结果文件读取、sweeper、R3、
  L2 回填都只用它。测试经 `RouterConfig.ClaudeDir` 注入。

**RunDir 只由 board 经一个函数生成**。Tracker 在 `internal/cli` 的纯逻辑叶子包里，没有 ProjectsRoot，只记原串
（`LaunchTranscriptDir`、`RunID`、`SessionID`，§4.2）。board 在发布时只**登记**新出现或来源变化的条目（§5.8 wake 第 7 步），由 I/O 派发在
**锁外、readLoop 外、表事务外**调用下面的函数，结果回到 b.mu 内复核来源元组与代数后才写入（§5.8"RunDir 解析"）。v3 写的是"board 在发布时调用"——
发布在 b.mu 内、在 readLoop 上，bind / procEnded 还在 `r.ss.Update` 里，而这个函数要做 EvalSymlinks 与 Lstat 祖先遍历。board 的输入：
`projectsRoot` 在创建 board 时由 `HistoryIO.projectsRoot` 传入；workspace 每次 bind 由 `bookWorkflows` 传入 `s.Workspace()`；ProjectDir =
`filepath.Join(projectsRoot, claudefs.ProjectSlug(workspace))`，纯字符串运算，在解析任务里求出。函数：

```go
// internal/claudefs/workflow.go
// ResolveWorkflowRunDir 返回已校验的 run dir 与 result 文件路径；任何一步不过即 ok=false。
func ResolveWorkflowRunDir(projectsRoot string, src WorkflowRunSource) (runDir, resultFile string, ok bool)

type WorkflowRunSource struct {
    TranscriptDir string // 来源 1：live tool_use_result.transcriptDir
    ProjectDir    string // 来源 2/3：<projectsRoot>/<slug>，由 board 从 session workspace 求出
    SessionID     string // 必须过 IsValidSessionID
    RunID         string // 必须过 IsValidWorkflowRunID
}
```

它依次做：ID 正则 → `filepath.Join` → `EvalSymlinks`（`projectsRoot` 已在 `ResolvedProjectsRoot` 里解析过，两侧形态一致，满足
`PathContainedInRoot` 的 CONTRACT）→ `rel, ok := osutil.RelUnderRoot(resolvedCandidate, projectsRoot)` → 结构检查（解析 `rel`，见来源 1）→
**按 projectsRoot 的拼写重拼** `runDir = filepath.Join(projectsRoot, rel)`（`resultFile` 同样由 `rel` 推出 `<slug>/<sid>/workflows/<runId>.json` 后重拼）。

- **参数顺序**：`PathContainedInRoot` 的签名是 `(resolved, root string)`（`osutil/pathroot.go:19`），候选在前、root 在后。v3 写成
  `PathContainedInRoot(projectsRoot, …)`：照抄的话问的是"root 是否在候选之下"——合法的 run dir 一律被拒（功能静默 404），反过来 projectsRoot 的
  任何祖先都能过这道门（来源 1 的 basename 正则与结构检查仍会挡住，但这道门本身是错的）。
- **`osutil.RelUnderRoot(resolved, root) (rel string, ok bool)`**（新增，与 `PathContainedInRoot` 同文件、同一套判定）：字节前缀命中时 `rel` 为前缀
  之后的部分；字节前缀不命中、`sameFileAncestor` 在某个祖先上按 inode 命中（大小写不敏感文件系统，EvalSymlinks 保留了用户输入的大小写）时，
  `rel` 为该祖先**之下**的分量。`PathContainedInRoot` 改为 `_, ok := RelUnderRoot(...)` 的薄包装，行为不变。
- **为什么要重拼**：inode 兜底放行的恰是大小写与 root 不同的路径，而下游的门全按字节前缀判断——agentevents 的 `jsonlPathUnderAllowedRoot`
  （`handler.go:309-346`，§8.2 原样复用）与结构检查都会把它判为越界，于是"已过 containment 的 run dir"在 drill-in 时 404。重拼之后 RunDir 的前缀与
  projectsRoot 逐字节相同，下游无需各自处理大小写；PR-4 另把 agentevents 的最终比较也换成 `PathContainedInRoot`（与 tailer registry 一致），作为纵深。
  实际只有来源 1（CLI 给出的 transcriptDir）可能与 root 大小写不同；来源 2、3 由 projectsRoot 拼出。

结果写进 board 的 `resolve[task]`、随 Published 发布为 `Workflow.RunDir`（board 是唯一写者），所有读者——session 层的结果文件读取、
sweeper、R3、L2 回填，server 侧的 §6.2 HTTP 与 §8.2 drill-in——都只用 board 发布的 RunDir，不各自再拼路径；RunDir 为 "" 时视为"尚未就绪"。
**打开文件时不再信任这个绝对路径的中间分量**：每次读取在锁外 `os.OpenRoot(projectsRoot)`，再以 `rel` 经 `osutil.OpenRegularIn` / `OpenDirIn`
打开（§10），中间目录在解析之后被换成指向 root 外的 symlink 也逃不出去。

run dir 来源优先级：

1. `WorkflowLaunch.TranscriptDir`（live，`tool_use_result.transcriptDir`）——校验：
   `EvalSymlinks` 后在 ProjectsRoot 下（`RelUnderRoot`）；basename 过 runID 正则；`rel` 的结构必须是
   `<slug>/<sid>/subagents/workflows/<runId>`，`<sid>` 过 `claudefs.IsValidSessionID`，
   `<runId>` 与 `WorkflowLaunch.RunID` 一致。
2. `Ref.RunID` + `Ref.SessionID` + 项目目录——两个 ID 从 sessions.json 读回，**重新过正则**（R0 时一次、解析时再一次），不信任盘上的值。
3. §5.9 R3a 的按 agentId 磁盘定位（找到的目录名同样过 runID 正则，再走上面的函数）。

`internal/claudefs/path.go`（:109-126 之后）新增的拼接 helper 对非法输入**返回 ""**（调用方把 "" 当作"无路径"），保证即便有人绕过
`ResolveWorkflowRunDir` 直接调用，也拼不出越界路径：

```go
func WorkflowRunsDir(subagentsDir string) string               // <subagents>/workflows
func WorkflowRunDir(subagentsDir, runID string) string           // runID 非法 → ""
func WorkflowJournal(runDir string) string                     // runDir/journal.jsonl；runDir == "" → ""
func WorkflowResultFile(projectDir, sessionID, runID string) string // <projectDir>/<sid>/workflows/<runID>.json；任一 ID 非法 → ""
func IsValidWorkflowRunID(id string) bool                      // ^wf_[A-Za-z0-9-]{1,64}$（实测 wf_<8hex>-<3hex>）；PR-4 落地（PR-8 的 R0 要用）
func ResolvedProjectsRoot(claudeDir string) string              // 见上
```

`usage.go:227-232` 改用 `WorkflowRunsDir` 共享常量。agent 文件复用 `SubagentJSONL/SubagentMeta`
（以 runDir 为目录）。

**session-id 漂移**：run dir 挂在 launch 时的 session id 下，`Linker.SetContext` 每次 init 都会
覆盖 `parentSessionID`（`subagent/link.go:203-213`）。因此 `Workflow.SessionID` / `Ref.SessionID`
在 launch 时固化，drill-in 不依赖 linker 当前上下文——也绕开了 linker "missing context" 的 202
窗口（本机日志 576 次 `Resolve bailing — missing context`）。

### 8.2 HTTP：复用 `agent_events`

`agent_id` 形如 `a` + 16 hex（17 字符小写），天然通过 `taskIDRe ^[a-z0-9]{1,32}$`
（`agentevents/handler.go:35`）。所以**不加新参数**。`HandleAgentEvents` 在 key/task_id 校验与
remote-node 分支（:121-140）**之后**、`linkerForSession` / `linker == nil → 404`（:142-146）**之前**
查 board：`agentevents` 的 deps 增 `WorkflowAgent(key, agentID string) (workflow.AgentLoc, bool)`
（实现为 `router.SessionFor(key).WorkflowAgent(id)`）。这样 R4 恢复、进程尚未 spawn 的 session
也能 drill-in 已知的 workflow agent。

- 命中且有 agentId（当前或历史 attempt），但 board 的 RunDir 尚未解析完成（§5.8"RunDir 解析"）→ **202 `{"status":"pending"}`**（同下面"未落盘"）。
- 命中且有 agentId、RunDir 已就绪 → `jsonlPath = SubagentJSONL(runDir, agentID)`（runDir 来自 board、已按 projectsRoot 拼写重拼，§8.1），过既有
  `jsonlPathUnderAllowedRoot`（:165；PR-4 起最终比较用 `PathContainedInRoot`）：
  - 经 root 锚定的 `osutil.OpenRegularIn(root, rel, 0)`（§10）打开：不存在或文件为空 → **202 `{"status":"pending"}`**（agentId 先于 transcript 落盘；复用
    `agent_view.js:236-258` 的有界重试，而不是 404 触发 toast + `switchTo(null)`，:259-262）；
  - 存在但不是 regular file（含 FIFO、symlink）→ 404；
  - 首行身份校验：`subagent.ReadFirstLineIDs(r io.Reader) (sessionID, agentID string, err error)`（新导出
    helper，`internal/subagent/link_scan.go` 旁，**PR-10 落地**）：`json.NewDecoder(io.LimitReader(r,
    limits.MaxStreamJSONLine)).Decode(&struct{SessionID, AgentID string})`，只解两个键、跳过巨大的
    `message`，首行 > 32KiB 也能读（`readFirstLineMeta` 的 32KiB 缓冲与 `errFirstLineTooLong` 不
    变，它仍服务 linker）。`sessionId == Workflow.SessionID` 且 `agentId == agentID`，否则 404。同文件另导出
    `ReadFirstPrompt(r io.Reader, maxRunes int) (sessionID, agentID, prompt string, err error)`：**一次**有界解码首行，同时取两个 ID 与
    `message.content`（字符串或 text 块数组）的文本，文本先 redact 再截断——§6.2.2 只用它、不再另调 `ReadFirstLineIDs`（v3 每次请求解两遍首行）；
    两者的结果都按 `(agentId, dev, ino)` 缓存在 board 里（LRU 64，§5.3；首行永不变化，键里带 inode 是为了文件被替换后不沿用旧的校验结果），命中缓存时 drill-in 只需打开文件并 Fstat、不再解码首行；
  - 然后用**同一个 fd** 构造 transcript reader 分页。仅此还不够：`TranscriptReader` 在没有缓存 fd 时（`openOrReuse`）与**每次零字节轮询**
    （`reprobeRotation`，`subagent/transcript.go:71-109,129-158,237-262`）都会按路径 `os.Stat`（跟随 symlink）再 `os.Open`（O_RDONLY，遇 FIFO 阻塞），
    而且持有 `r.mu`；`Close()` 也要取 `r.mu`。rm + mkfifo 之后，下一次零字节轮询就阻塞在 `os.Open` 里、reader 永远拆不掉。所以 PR-13 改的是
    reader 本身，而不只是加一个构造入口：
    - 新构造 `subagent.NewTranscriptReaderFrom(f *os.File, open func() (*os.File, error))`：首个 fd 来自上面的校验；**之后每一次重新打开**
      （`openOrReuse` 无 fd、`reprobeRotation` 发现 inode 变化）都经 `open()`，不再直接 `os.Open`；
    - 旋转探测从 `os.Stat` 改为 `os.Lstat`（不跟随 symlink、对 FIFO 不阻塞）；探到的不是 regular file → 按"文件已消失"处理（关闭旧 fd、返回 not-exist），
      不去打开它；
    - `NewTranscriptReader(path)` 保留，默认 `open = func(){ return osutil.OpenRegular(path, 0) }`——既有 Agent drill-in 与 Agent tailer 一并获得保护；
      workflow agent 用 board 提供的 root 锚定 opener（`OpenRegularIn(root, rel, 0)`）。
- 命中但从未有 agentId（queued）→ 404（不返回 202，避免前端 20×250ms 白等，`agent_view.js:148,237-260`）。这是 drill-in 端点
  `agent_events` 的语义，与 §6.2.2 预览端点对 queued 返回 200 不同，前端也不会对 queued 行发起 drill-in（§7.3 不可点）。
- 未命中 → 走原 linker 路径，行为不变。

**不**经过 `Resolve` / `resolveByTaskIDFast`：二者都会 `fireCallbacksDropLock`，触发 server 侧
OnResolve 为每个 agent 自动起 silent tailer（`wshub_agent.go:69-81`）以及按 toolUseID 的
`SetAgentInternalID`（会写到 Workflow 的 task_start 行上，`process_event_query.go:98-104`）。
workflow agent 的映射只存在 board 里，不进 linker 的 `byTaskID/byName`。

### 8.3 WS：复用 `agent_subscribe` + tailer

- `handleAgentSubscribe`（`wshub_agent.go`）在 `SessionFor` 命中之后、`SubagentLinker() == nil →
  no_linker`（:130-138）**之前**查 board；命中则同 §8.2 校验，文件未就绪 → `agent_subscribe_rejected
  {reason:"pending"}`（既有原因，客户端已会重试；RunDir 尚未解析完成同样走这里）；就绪 → `ensureTailer(key, agentID, "", jsonlPath, open)`
  （`agent_tailer_registry.go:179`；新增末参 `open func() (*os.File, error)`，nil = 默认 `OpenRegular(path, 0)`），`ensureTailer` 用
  `subagent.NewTranscriptReaderFrom(f, open)` 建 reader，`f` 是 §8.2 校验过的那个 fd——v3 按路径 `NewTranscriptReader(jsonlPath)`
  （:214）新建，§8.2 校验过的 fd 根本到不了 tailer，之后的每次重新打开也都是裸 `os.Open`（§8.2）。这一点对整个 registry 都要紧：registry 只有
  一个 `pollLoop` goroutine，串行推进全部 tailer（`agent_tailer_registry.go:95-110`），一个阻塞在 FIFO 上的 `Tail()` 会让所有 agent 的实时
  transcript 一起停住，`finalize` 里的 `reader.Close()`（`agent_tailer.go:251`）也会卡在 `r.mu` 上。v2 写"既有原因，客户端已会重试"不实：`agent_view.js:535-538` 收到 `pending` 只是 `break`，
  而 `subscribeCurrent` 只在 HTTP 拉取成功之后才调用（:280-288）——HTTP 200 已意味着 jsonl 存在且非空，WS 的 `pending` 实际几乎不可达。
  真出现时（文件在两次检查之间被删）客户端按 `capacity` 的方式回退到既有的 3s HTTP poll（:517-519），PR-13 在 `agent_view.js` 里给
  `pending` 分支补这一行。
- **前置修复（独立 PR）**：tailer registry 的 `allowedRoot` 是 operator workspace，而不是
  `~/.claude/projects`：`cmd/naozhi/main.go:415 AllowedRoot: workspace` →
  `build_dashboard.go:113` → `wshub.go:156 newTailerRegistry(opts.AllowedRoot)`，`ensureTailer`
  （`agent_tailer_registry.go:186-191`）拒绝 workspace 之外的 jsonl。本机 `cwd` 为
  `/Users/zhaokm/Workspace`，所以所有 transcript 都会被拒，客户端拿到 `capacity` 静默降级为 3s HTTP
  poll（`wshub_agent.go:166-172`、`agent_view.js:517-519`）——由代码推断，测试全用
  `newTailerRegistry("")`（`agent_tailer_test.go:94-448`）未覆盖生产配置。修法：`build_dashboard.go:113` 改为把 §8.1 的
  `claudefs.ResolvedProjectsRoot(claudeDir)` 放进 **既有的 `HubOptions.AllowedRoot`**——在 `internal/server` 里它只被
  `wshub.go:156 newTailerRegistry(opts.AllowedRoot)` 读取，换掉值不影响别处；`build_handlers.go` / `build_dispatch.go` 用的是各自
  struct 的 `AllowedRoot`（`w.allowedRoot`，operator workspace），不动。这样不加 HubOptions 字段（`hubOptionsFieldBaseline = 16`，
  `-mode fail`），也不给 Router 加访问器（Router 方法预算）。registry 与 agentevents 的 `jsonlPathUnderAllowedRoot`
  （`handler.go:309-346`）的最终比较都改用 `osutil.PathContainedInRoot(resolved, root)`（候选在前），两处对大小写不敏感文件系统的判定一致。
  `wshub_wired_linkers_test.go:89-92` 钉的 `tailers.allowedRoot == HubOptions.AllowedRoot` 仍成立，只是断言的值改为 projects root。
- **按需、不自动**：workflow agent 不注册 OnResolve，不自动起 tailer；50 上限
  （`agent_tailer.go:33`）与普通 Agent 共享，超出走既有 capacity → HTTP poll 降级。
- **agent_done**：workflow agent 不会收到 per-agent `task_notification`（parent 的通知携带的是
  workflow 的 task_id）。tailer 在构造时拿一个 `doneFn func() (status string, done bool)` 闭包，读
  board 的 `Published`（O(1)，经 `byAgentID`，所以 agentId 粘滞后仍能找到）：当前 attempt 的行处于
  done/failed/skipped/stopped，或该 id 是历史 attempt（`Current=false`）→ done。200ms poll 循环在 EOF
  且 done 时发 `agent_done` 并关闭。无新回调，不碰 `SetOnAgentTaskDone`。workflow 终态时关闭该 run
  的全部 workflow tailer。
- `enrich()`（`agent_tailer_registry.go:145-172`）只覆盖 `snap.Subagents`，不处理 workflow（不需要）。

### 8.4 前端

- 行点击（`data-action="wf-open-agent"`，agentId 取自 `data-agent-id`，不经 `.rb-agent-row[data-task]` 的委托监听，§7.3）→
  `nzViews.agent.switchTo(agentID, {label, crumb: "<workflow> · <phase>"})`：`switchTo`
  （`agent_view.js:182`）增可选第二参，因为面包屑查找 `findAgentByTaskId`（:108）只搜
  `turnState.agents`。WS `agent_event` 过滤 `task_id === state.activeTaskID` 不变（id 即 agentId）。
- Esc 返回、parent 事件在 drill-in 时不进 DOM 等行为全部沿用（`agent_view.js:652-664`、
  `event_stream.js:792`）。
- 默认 drill 当前（或粘滞的最后一个）agentId；行内 attempt 徽标（`×2`）展开后列出 `prev_agent_ids`，
  点击同样 `switchTo`（Q6）。

## 9. 可选：合并冗余的单行 task_progress ring 条目

在 Tracker 已标记 `ev.WorkflowTask`（§5.7 `IsWorkflowTask`）的前提下，`EventEntriesFromEventAt`
（`process_event_format.go:45-57`）：

- workflow 的 `task_progress` / `task_updated` → **不产生 EventEntry**（不占 ring 槽、不持久化、不覆盖
  `lastActivitySummary`）。非 workflow 任务（含带 `summary` 的 mcp_task / local_agent progress）不受影响。
- workflow 的 `task_started` 保留（`KindTaskStart`，Summary=description，TaskType=local_workflow）；
  `task_notification` 保留为 `KindTaskDone`，Summary 用 `ev.TaskSummary`（修 "task_notification" 字面量）、
  TaskType=local_workflow。
- 非 workflow 的 `task_updated` 也修：不再写 Summary=`task_updated`，`Status` 取 `patch.status`。
- 前提：保活已改看 `board.LastObservedAt`（§5.8），否则 ring 不再刷新 `LastEventAt`
  （`eventlog_append.go:136,316`）会让 Cleanup 提前过期。
- 收益：消除 §1.2.4 的洪水（最大 log 70% 字节是 task_progress）；首页不再因 500 条内部条目
  `eventLastNVisibleCtx` 早退而零气泡（`managed_query.go:675`）；rotation 不再挤掉真实对话
  （`persist/rotate.go:18`、`persister.go:90`）。
- 前端顺带修 turn timer 缺陷：`applyEventToTurnState` 总是先 `startTurnTimer()`
  （`running_banner.js:234-235`），空闲期 progress 会设置 `turnStartTime`，下一轮 send 时
  `startTurnTimer` 早退（:83-89），elapsed 从第一条后台事件算起。改为：session 非 running 时
  `task_progress/task_done` 不启动 timer。
- 兼容：`process_extra_test.go:1395-1440` 钉住的映射需同步更新；`kinds_test.go` 不变（不新增 kind）。

此项单独成 PR、放在最后，因为它是唯一改变既有可见行为（历史里不再有 workflow progress 行）的改动。

## 10. 安全与隐私

- **信任边界不变**：任何已认证 WS client 可订阅任意 key（`wshub.go:26-29`），workflow 数据与 transcript
  同级敏感，不新增边界；所有 HTTP 走 `apiChain`，WS 消息在 `c.authenticated` 之后
  （`wsclient.go:210-288`）。
- **Redaction**：`ForWire` 只 redact EventEntry 的 Summary/Detail（`clievent/wire.go:51-66`）。workflow
  的 label、phase title、last tool name / summary、error，以及 workflow 层的 Name、Description、Current、NotifySummary
  在 **Tracker 规范化时**先 `RedactSecrets` 原串再截断（§4.3），WireView / `Summary`（进 `/api/sessions`）/ `Ref`（进 sessions.json）都从已脱敏字段派生；
  REST 体里的 prompt、result、logs、resultPreview 在 board / handler 读盘处同样先 redact 后截断。泄漏测试
  覆盖每个字符串字段（含 Summary.Name/CurrentPhase、Ref.Name、Current 里藏在未截断 label 中的密钥），并有一个密钥跨截断边界的用例。
- **绝不上 wire**：`RunDir`、`transcriptDir`、`scriptPath`（全路径）、`output_file`、脚本文本、`args`。
  **既有泄漏**：今天 Workflow tool_use 的 Detail 是原始 input JSON 前 300 字节（§1.1），脚本源码已经进了
  ring、持久化与 wire；`ForWire` 的 `RedactSecrets` 只遮密钥、不删脚本。PR-5 的 `case "Workflow"` 修掉新
  产生的条目；已持久化的历史条目保留原样（NG7）。
- **路径安全**：client 永远只提供 `task_id` / `index` / `agent_id`，路径由服务端从已校验来源拼出；
  runID、sessionID 与 agentID 先过正则（`IsValidWorkflowRunID`、`IsValidSessionID`、`agentHexRe ^[A-Za-z0-9]{8,64}$`，
  `subagent/link.go:28`）再 `filepath.Join`；EvalSymlinks + `PathContainedInRoot`，全部收敛在 `claudefs.ResolveWorkflowRunDir`
  一个函数里，RunDir 只由 board 写（§8.1）；sessions.json 读回的 Ref ID 重新校验。
- **打开文件：一个 helper 管所有 run 目录下的文件**。v2 只给结果文件 O_NOFOLLOW + Fstat；journal.jsonl（任何有 Bash 的 workflow agent
  都能写）、`agent-*.meta.json`（L2）、agent jsonl（Lstat 后另行 `os.Open`，TOCTOU）都没有。而且 **O_NOFOLLOW 挡不住 FIFO**：
  `O_RDONLY` 打开 FIFO 会阻塞到出现写者——现有 `files_open_unix.go:17` 与 `HandleToolResult`（`agentevents/handler.go:248-263`）
  都是先 open 后 Fstat，同样受影响。一个 mkfifo 落在 journal.jsonl / `wf_<runId>.json` / meta 上，就能永久挂住 HTTP handler goroutine，
  加上"每个 board 至多一个在途读取"（§5.6），还会永久卡住该 board 的 sweeper 与对账，workflow 一直 running、一直钉住保活直到
  `workflowPinMax`。新增：

  ```go
  // internal/osutil/open_regular_unix.go（PR-3）
  // OpenRegular 以 O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC 打开（FIFO / 设备不阻塞），Fstat 该 fd：
  // 非 regular file → ErrNotRegular；maxBytes > 0 且 Size > maxBytes → ErrTooLarge。调用方经返回的同一个 fd 读。
  func OpenRegular(path string, maxBytes int64) (*os.File, os.FileInfo, error)

  // 同文件（PR-9）：root 锚定的变体。root 由调用方在锁外 os.OpenRoot(projectsRoot) 得到、用完即关；rel 是 §8.1 的 RunDir / resultFile
  // 相对 projectsRoot 的部分。os.Root（Go ≥ 1.24）拒绝任何分量逃出 root 的路径——中间目录在 RunDir 解析之后被换成指向 root 外的
  // symlink 也打不开，而 O_NOFOLLOW 只管最后一个分量。
  func OpenRegularIn(root *os.Root, rel string, maxBytes int64) (*os.File, os.FileInfo, error) // root.OpenFile(rel, 同上 flag) + Fstat
  func OpenDirIn(root *os.Root, rel string) (*os.File, error)                                   // 再加 O_DIRECTORY；R3a 的 ReadDir 用它
  ```

  （regular file 上 O_NONBLOCK 对读无影响；同仓先例：`files_publictmp.go:87-94` 拒收 socket / FIFO / 设备，#1688。非 unix 平台
  按 `atomicfile_unix.go` / `atomicfile_nonunix.go` 的分法给一个 Lstat + Open + Fstat 的退化实现。）PR-9 先用测试确认 `Root.OpenFile`
  把 O_NONBLOCK 透传给最后一个分量（FIFO 不阻塞）；若不透传，`OpenRegularIn` 退回"`OpenRegular(filepath.Join(projectsRoot, rel))` 前再对
  `filepath.Dir` 做一次 EvalSymlinks 并要求等于已解析的父目录"，剩下的竞态窗口记为已知限制。
  - **尺寸上限**（`maxBytes`，超过即 `ErrTooLarge`）：结果文件 16MiB、journal 64MiB、meta.json 64KiB；agent jsonl 不设上限（分页读）。
  - **读窗口**（不是尺寸上限，`maxBytes = 0` 打开后 `ReadAt` 尾部）：主 transcript 尾窗 64KiB、必要时放大到 1MiB（§5.10）。v3 把它和尺寸上限
    列在一起，照抄会让每个真实 JSONL 都 `ErrTooLarge`。
  - **重新打开也算打开**：首次打开之外的每一次重新打开都必须经 `OpenRegular` / `OpenRegularIn`——`TranscriptReader` 的 `openOrReuse` /
    `reprobeRotation` 原来是 `os.Stat` + `os.Open`（§8.2），被换成 FIFO 后会阻塞在持有 `r.mu` 的路径上，拖住整个 tailer registry（§8.3）。
  - **目录**：R3a 的目录经 `OpenDirIn` 打开（被换成 FIFO / symlink 时立即报错），用 `f.ReadDir(257)`，超过 256 项即放弃并计数，而不是
    `os.ReadDir` 全量读入后再判断；条目只接受 `e.Type().IsRegular()` / 真目录（同 `usage.go:205-210`）。
  - 所有后台读取经 board 的有界 I/O 派发（§5.6(6b)：每 board ≤ 2、全局 8，卡死的任务继续占槽位）。`HandleToolResult` 与 `files_open_unix.go`
    的同类问题不在本 RFC 范围，另开 issue。
  - **测试的平台约束**：每个用到 `syscall.Mkfifo` 或依赖 O_NOFOLLOW 语义的测试都放在 `*_unix_test.go` 里并加 `//go:build !windows`
    （先例 `internal/gitinfo/open_unix_test.go`、`internal/dashboard/project/files_mkfifo_unix_test.go`、
    `internal/cli/wrapper_enforce_clipath_unix_test.go`）——必过的 `build-windows` job 跑 `go vet ./...`，vet 会对测试文件做类型检查
    （`ci.yml:436-457`），windows 上没有 `syscall.Mkfifo`。涉及的包：osutil（PR-3、PR-9）、subagent（PR-13）、session（PR-9 的 sweeper / R3a）、
    agentevents 与 server（PR-13）。
- **DoS**：stream 解码有 10MiB 行上限（超过的快照行被跳过并置 `snapshot_dropped`，§5.7；shim 侧 10MB 的超长行会卡死 session，属既有行为，§13 R14），journal 扫描单行 16MiB（`limits.MaxStreamJSONLine`）；Tracker 与 board 有 §5.3 的硬上限（含 phase 200、后台 I/O 在途 8）；磁盘扫描有目录数 / 文件尺寸上限；结果文件
  每个终态只解析一次并缓存、journal 用偏移索引；WS 帧 ≤ 192KiB 且有背压跳过；**HTTP 由新端点自己的
  per-IP limiter 覆盖**（`apiChain` 无限流，`sendLimiter` 只挂在 send 系 handler 上）。
- **XSS**：前端只用 `textContent` / `setAttribute`，不新增 HTML sink。

## 11. 测试计划

### 11.1 Golden fixture

- 把 `/tmp/nz-wfprobe/stream-sample.jsonl` 复制为
  `internal/cli/workflow/testdata/probe-3agent.jsonl`，脱敏（`/Users/zhaokm` → `/home/u`、
  去掉 init 帧里的 MCP 清单）；对应的 `wf_2997921d-435.json`、`journal.jsonl`、一个 agent jsonl
  首两行一并放入 `testdata/run/`。
- 手写的小 fixture（按 §1.2.2 的 CC 构造器形态）：安全分类器拦截项（`state:"error",blocked:true`，
  无 agentId）、排队 catch 项、限流重排队（running → 无 agentId 的 `start` → 撤销 `error`）、重试换新
  agentId、用户跳过、`task_notification.status:"stopped"`、resume（同 runId 新 taskId，盘上为旧 taskId
  的结果文件）、`"workflow_progress":null`、mcp_task 带 `summary` 的 `task_progress`。
- 大快照用包内测试 helper `func bigSnapshot(n int, opts bigOpts) []byte`（放在 `internal/cli/workflow` 与 `internal/cli`
  各自的 `_test.go` 里，或前者导出给后者的 `workflowtest` 子包），在测试与 bench 里现场合成 400-agent / 2000-agent 快照（`opts` 可让
  lastToolSummary 全部含 `=`，走 redact 的正则分支），不入库大文件。v2 的 `testdata/gen_big.go`（`//go:build ignore`）在 `go test`
  时不会运行，产物又不入库，依赖它的 bench 与 2000 行用例拿不到输入。

### 11.2 单元

| 层 | 用例 |
|---|---|
| `ReadEventInto` 解码 | probe 每行映射正确；`workflow_progress` 某项 `tokens:"x"` → `Partial`、其余项与 description 完好；整体非数组 → `Failed`、Event 其余字段完好；**`[1,2]`、`"index":"3"`、`"type":5`、`"agentId":1` → `Failed`**（不是 Partial）；**`tokens:"x"` 在前、`index:"3"` 在后的两项 → 身份复核降为 Failed**；**`{"workflow_progress":[{"tokens":"x"}],"status":5}` 与键序反过来的同一帧都返回 err**（被遮住的错误经第二遍暴露）；键缺失与 `null` → nil；`[]` → 非 nil 空；workflow_progress 之外的类型错仍返回 err；`async_launched` 定向解码；`tool_use_result` 非 workflow 时为 nil；Fuzz（`FuzzReadEventWorkflow`）永不 panic |
| hook 快路径 | `"label":"hook_tests"` 的快照**不再被跳过**；assistant tool_use `input` 含 `{"subtype":"hook_started"}` / `{"type":"control_response"}` 的帧照常交付（旧代码必失败）；真实 hook / control_response 帧仍走快路径；键序变化的 control_response 经兜底仍产出 ack |
| `FormatToolInput` | Workflow input 含 `script` / `args` 时 Detail 不含其任何子串；有 `scriptPath` 时只出 basename |
| eventCh / readEventBuf | 出 `deliverEvent` 的 Event 与喂完快照帧后的 `p.readEventBuf[*]`，`WorkflowProgress == nil`（照 `process_live_version_test.go:25` 的接缝写法）；**killCh 已关闭时喂一张快照帧**（dispatch 提前 `return shimDispatchReturn`）后 `readEventBuf[*]` 同样为 nil |
| 超长行（§5.7 L1''） | fake shim 写一个 envelope > 10MiB 的已知 workflow 快照行：readLoop 跳过、Process 不结束、该 workflow `Degraded=snapshot_dropped` 且保留旧行、`workflow_lines_oversize` +1；截不出 task_id 的超长行只计数；之后一张正常快照清除标记 |
| Tracker 规范化 | 规范化表逐行（含 blocked / 排队 catch / 限流撤销 → failed，skipped，行 6 的 B → queued）；限流重排队后 `AgentID` 粘滞、`State=queued`；重试换 id 进 `PrevAgentIDs`（≤8）；先 redact 后截断（密钥跨 200 rune 边界仍被遮）；Name / Description / Current / NotifySummary / RawState / RawStatus 的上限；**记忆化**：原串不变时不再调用 `RedactSecrets`（计数接缝），原串变化时重算；Partial 帧身份复核不过时保留旧行 |
| Tracker 生命周期 | probe 回放后状态与 `wf_*.json` 一致；StartedAt：live task_started 取观测时刻、SeedFromReplay 不取观测时刻、结果文件 `startTime` 覆盖其余来源；description-only 帧只改 header 且不清 agent；`task_updated` 先于 notification 置终态；notification `stopped` → killed；终态后 running→stopped；终态后 progress 被忽略；两个 workflow 交错；2000 行 `AgentsCapped`；withheld（`phase N`、无 promptPreview）；未知 state/status 透传 |
| `IsWorkflowTask` / L3 | `a…` id 的 task_progress 带 `summary`、无 task_started/workflow_progress → **不建条目**；带 `subagent_type` → 否；Ref 已知的 task_id → 是；`background_tasks_changed` 帧不影响判定 |
| 结果文件合并 | taskId 不匹配（resume）→ 视为不存在、workflow 仍 running；匹配 → 覆盖后迟到的 notification 不改 totals；**StartedAt 为重启时刻（seed 的旧实现）而文件 `startTime` 早数小时 → 仍被采纳**（v2 的 StartTime 条件回归） |
| SeedFromReplay | 只解最新快照；**总解码次数为 O(task 数)**：ring 绕回、10k 行里绝大多数是同一 task 的小 task_progress 时，每 task 每帧类只解 1 行（解码计数断言，v3 只数快照解码）；running 的 workflow（无终态帧）同样只解 O(1) 行；`SeedWrapped` 正确；终态在快照之后；`task_started` 已被淘汰；50MiB 合成 replay 的解码次数与 `AllocsPerRun` 上限（不用挂钟） |
| reconnectVerdict | 末尾 workflow task 帧 → 不再 midTurn；前台 Agent 中途仍 midTurn；task-notification 自启 turn（init 在后）→ midTurn；只剩中性帧且 ring 已绕回 → `unknown` → resolver（`startReadLoop` 之前调用，计数接缝断言顺序）读 JSONL 尾 `end_turn` → idle、否则 midTurn（**5MB 的 JSONL fixture**：尾窗是读窗口、不是尺寸上限）；末条记录 200KiB → 窗口放大到 1MiB 后裁决、> 1MiB → midTurn；resolver 为 nil → midTurn；**live result 紧随 `startReadLoop` 到达时仍回到 Ready**；中性帧不被解码（计数） |
| linker | `local_workflow` 三处都不 Resolve；InjectHistory：带 TaskType 的 progress、遗留 `w`+8 孤儿 progress 都不 Resolve；`a…` 孤儿仍 Resolve |
| Board 并发 / 锁序 | **在 `r.ss.Update` 回调内 bind 一个含 running workflow 且 Tracker 领先 `applied` 的 proc：不死锁，且随后恰好一次 structural 通知**（v2 设计下死锁）；readLoop wake 与事务内 bind 交错（`-race`，10k 次）无死锁；事务内 `evictOldest` 调 `workflowPinned` 不取 b.mu（接缝断言）；`SetOnEnd` 在事务内同步投递时 procEnded 不死锁；同一 proc 重复 bind 为 no-op；bind 时的 Load 与 readLoop 唤醒竞速、结果文件合并与 Observe 竞速：最终发布态等于 Tracker 最新 Set；旧 Version 永不覆盖新；被放弃的 reattach proc 不发布；旧 proc 解绑后的唤醒被忽略；bind 时旧 proc 的 running 折入 retained 为 snapshot_stale、新 Tracker 同 task_id 覆盖；**RunDir 解析不在锁内**：计数型 fake resolver 下，`r.ss.Update` 内 bind 与 readLoop 上 wake 都零次同步调用；resolver 阻塞在 channel 上时 wake / bind 照常返回；解析途中来源元组变化 → 旧结果丢弃；未解析完成时 drill-in 202、sweeper 跳过 |
| Board 版本 | Ref 条目（R0）被版本更低的 Tracker 条目替换：wire version 仍递增、内容未变的行 Rev 不变、变化的行 Rev 前进；shim 重连换 Tracker 同上；retained 被 board 改为 interrupted / snapshot_stale / result_file 时版本递增；CoW 共享切片不逐行比较（计数）；`agentEqualIgnoringRev` 的字段数钉（`reflect` NumField）；只有 `PrevAgentIDs` 变化也推进 Rev；某 index 消失 → `RowsGen` +1 且全部行盖新 Rev |
| Board 进程结束 | `ProcessEnd{}`（cli_exited / Kill）→ interrupted；`ShimLive` → snapshot_stale、LastObservedAt 冻结、status 仍 running、不发终态；`Detached` → 同 ShimLive；ShimLive 之后重接的 Tracker 带同 task_id → 回到 live、无终态翻转；ShimLive 之后 90s 内无进程 → sweeper 判 interrupted（R4），再重接 → 覆盖回 running；**R5**（假时钟）：R0 恢复的 running 条目、bind 了一个从不报告它的新 proc → 未绕回 90s / 已绕回 10min 后变为 unknown（`unclaimed`）、不播报、`workflowPinned` 为假；之后该 Tracker 报告同一 task_id → 覆盖回 running；unknown 且超过 `workflowPinMax` 的条目不写进 Ref |
| Board 容量 | Tracker LRU 淘汰的终态条目仍在 Published（移入 retained）；**N > 5 次 bind 循环**（每次旧 Tracker 带若干终态）后 retained 终态 ≤ 5、非终态 ≤ 16、行总数有界，被淘汰条目的 `last` / 结果缓存 / journal 索引同时释放；Ref 恰为有界集合的 header；**live 非终态 20 个（Tracker 16 带行 + 4 header-only）+ retained 3 个**：多次 wake 后 live 全部保持 running、无 interrupted、无结构通知抖动，retained 3 个被删除；第 33 个 live 不建条目且计数；phase 第 201 个起被丢弃、`Degraded=too_many` |
| Board 其他 | respawn / rename 以指针携带（同一 `*workflowBoard`、epoch 不变）；**respawn 后被绑定的 board 就是携带来的指针**：`b.proc` 是新 proc、喂给新 proc 的快照出现在该 board 的 Published 里、旧 board 上没有残留的新 proc 绑定（v3 设计下新 proc 绑在一个随即被丢弃的空 board 上）；R0 的终态条目在 RunDir 解析后经一次 `MergeResultFile` 补齐行、version 前进；`/new` 新 epoch；Ref 持久化往返（含 `last_observed_at`；非法 SessionID / RunID 被丢弃）；Summary 预计算 O(1)；`Running()` 对 paused 为真、对 unknown 为假；sessions_update：结构变化立即调 structural 闭包、计数变化 ≤ 1/30s 调 `BumpVersion` 闭包且 trailing edge 补发（假时钟计数断言）；**计数变化后 `r.ss.Gen()` 前进而 dirty 不置位** |
| 保活 | `workflowPinned` 时 ReleaseIdleProcess 不释放；Cleanup 不过期、超过 `workflowPinMax` 后过期；scratch 从 `LastObservedAt` 起老化且 `turnOutstanding` 不变；`evictOldest` 优先非 workflow、无候选时回退驱逐 workflow session；`takeoverHasSlot` 与之一致 |
| 磁盘 / sweeper | runID / sessionID 正则（`..`、`/`、`wf_`、超长）；`ResolveWorkflowRunDir` 拒绝 root 外 / 结构不符 / ID 非法；path helper 对非法输入返回 ""；按 agentId 定位 run dir（`ReadDir(257)`，> 256 项放弃）；symlink run dir 被拒；**合法 run dir 被接受**（防参数顺序写反）；**候选是 projectsRoot 的祖先（如 `/`）→ 拒绝**；**darwin 上 transcriptDir 与 root 大小写不同 → 解析成功且 RunDir 按 root 的拼写重拼**（`RelUnderRoot` 的 inode 分支）；**journal / 结果文件 / meta.json 被换成 FIFO → `OpenRegularIn` 立即返回 ErrNotRegular、不阻塞**（带超时的测试）；**`subagents/workflows` 被换成 FIFO → R3a 的 `OpenDirIn` 立即报错**；中间目录在解析后被换成指向 root 外的 symlink → `OpenRegularIn` 拒绝；结果文件 / journal 超限被拒；journal 坏行跳过；在途读取卡住 30s 后该 board 至多补发 1 个、旧结果被丢弃；**挂死的 fake FS 上跑 100 个 30s tick：在途 goroutine 恒 ≤ 8**（v3 无上限）；journal 索引 singleflight（20 个并发请求只扫一遍）、文件被替换（inode 变）或变小后重建；sweeper（假时钟）：丢失 `task_updated` 的 run 在第一个满足"60s 无观测"的 30s tick 置终态；终态后文件迟到由重试补上；续跑中旧文件不被采纳；sweeper 是自由函数（`TestRouterBudget` 不变） |
| wsproto | full/delta / `workflow_set` 构造；Frames 示例全字段非零 |
| Hub `workflowPushLoop` | 先订阅后快照（快照与订阅之间的终态变化不丢）；首帧是 `workflow_set`（空 board 发空列表）；初始 full 无 agents；`trySendRaw` 失败不前进 `lastSent` 且重试后 delta 含遗漏行；纯 progress 1Hz；`len(c.send) > 8` 跳过非终态；**send 队列满、三个 task 同时终态：重试期间不调用 `trySendRaw`、`c.dropped` 不因 workflow 重试增长、client 不被断开**，队列降到 cap/2 以下后终态送达；rows_gen 变化 → 对该 task 发 full；task 被 board 淘汰 → 新的 `workflow_set`；每帧 ≤ 192KiB（2000 个 queued 行、2000 行同时变 stopped）且 `rows_omitted` 正确；eventPushLoop 阻塞在 resubscribe 时（进程分离）board 变化照样送达；unsubscribe / generation 接管后 ≤ 5s 退出且不再发送；`/new` 换到空 board 后发空的 `workflow_set`；suspended 分支无 loop；泄漏测试覆盖每个字符串字段 |
| HTTP | 未鉴权 401（自动）；非法 key 400；`rows=none` 与 `since` 同时给 → 400；`SessionFor == nil` 404；task 不在 board 404；remote node 404；**queued agent 200 只含 label、`transcript:false`**（v3 此处写 404，与 §6.2.2 矛盾）；**结果文件缺失 / 是 FIFO / RunDir 未解析 → `/workflow` 200 + `result_unavailable`、`/workflow_agent` 200 + `transcript:false`**（不是 404）；`rows=none` 不带行；`since=V` 只带 `rev > V` 的行、epoch 或 rows_gen 不符时 `rows_mode:"full"`；重启后缓存缺失的终态 workflow 首次 GET 即返回行（`MergeResultFile` 经 HTTP 路径）；结果/日志截断（logs 合计 ≤ 64KiB）；结果文件只解析一次（计数）；taskId 不匹配不返回 result；per-agent result 在 running 与终态下都来自 journal（同一 agent 前后一致）；响应带 `server_now` 与 `rows_gen`；prompt 首行 > 32KiB 仍可读；首行每个 `(agentId, dev, ino)` 只解码一次（计数）；限流 429 |
| drill-in | board 命中先于 linker nil（无进程的 session 可 drill）；transcript 未落盘 → 202 / `pending`；transcript 是 FIFO → 404 且不阻塞；**tail 中途把 agent jsonl rm + mkfifo**：下一次零字节轮询不阻塞、`Close()` 立即返回、registry 的其他 tailer 照常推进（`pollLoop` 串行，带超时断言）；既有 Agent tailer 同样受保护（默认 opener）；RunDir 未解析 → 202；首行 > 32KiB 仍校验通过；首行 sessionId/agentId 不符 → 404；历史 attempt agentId 可 drill 且 doneFn 立即 done；tailer 用共享 ProjectsRoot allowedRoot（新增生产配置用例，修 `newTailerRegistry("")` 盲区）；done 时 `agent_done`；WS `pending` → 客户端回退 3s poll |
| 前端 `workflow_state.js`（node --test） | §6.1 客户端表逐行：`workflow_set` 删掉不在列表 / epoch 不同的条目（`/new` 后清空）；full 在 epoch / rows_gen 变化时丢 rows、**相同时保留 rows**（内容未变的重订阅零请求，`rowsAt` 落后时恰一次 `since`）；fetch 在途时到达的 full 不另发请求、HTTP 响应 `H < version` 时 header 与 version **不倒退**；`version ≤ local` 忽略 header；`base > local` 触发 fetch；`base ≤ rowsAt < version` 合并行；`base > rowsAt` → `since` 拉取；rows_gen 变化丢 rows；fetch 在途时缓存并在 HTTP 落地后重放；`ensureRows` 与 full 帧同时触发时只发一次；兜底刷新：`!rowsLoaded` → `rows=none`、`rowsLoaded` → `since`（WS 断开 5s 轮询下折叠的 workflow 从不拉全量行）；`/workflow_agent` 的 404 不删条目；HTTP 之后的 delta 不再触发重拉（无限重拉回归）；`agents_capped` 不触发 fetch，`rows_omitted` 只触发一次；epoch 变化；429 保持状态、按退避（含 Retry-After）重试；404 删除条目；Summary 兜底判定（node 非 local 跳过、无 `workflow_set` 时以 Summary 裁剪）；`releaseRows` 后 `rowsLoaded=false` |
| bench | `BenchmarkReadEvent_WorkflowSnapshot398`：`-count≥5` 中位数 ≤ 1.5ms 且 ≤ 400KB/op（M 系列本机；CI 不跑，PR 描述贴数，含跳过路径对照）。**`BenchmarkObserve_Snapshot398`**（解码 + `Observe` + board `wake`，真实 398-agent 快照，稳态：上一张与本张只差几行）中位数 ≤ 2.5ms；**`BenchmarkObserve_Snapshot2000Eq`**（2000 行、lastToolSummary 全含 `=`）首张 ≤ 25ms、稳态 ≤ 5ms——稳态预算靠 §4.3 的记忆化达成，PR-6 / PR-8 实测后在 PR 描述里定稿 |

### 11.3 Race

`go test -race -count=20 -timeout 40m ./internal/cli/workflow/... ./internal/cli/ ./internal/session/
./internal/server/ -run 'Workflow'`：readLoop Observe 与 Snapshot / Hub / HTTP 并发 Load；结果文件合并与
Observe 并发；board 多写者唤醒与 bind（含在表事务内 bind、procEnded 与 wake 交错）；sweeper 与 bind 交错；锁外 RunDir 解析 / 结果文件读取的回写与 wake、bind、条目裁剪交错（来源元组 / 代数复核）；respawn 携带与旧 proc 的 ProcessEnd 交错；
workflowPushLoop 与 board 发布、generation 变化并发。

### 11.4 e2e（mock-server，不碰 :8180）

- `test/e2e/mock-server.js` 先补 WS 帧 127/64-bit 长度分支（`mock-server.js:1213-1219` 只有 7/16-bit，
  ≥ 64KiB 抛错），再加 `overrides.ws` 推 `workflow_state`（full → delta → 终态）与两个 REST 路由。
- 用例按落地的 PR 拆成两组（v3 让 PR-12 跑整个 §11.4，其中 drill-in 用例要等 PR-13 的 `switchTo` 第二参与 `wf-open-agent`）：
  - **§11.4a 面板 / store / 无障碍（PR-12）**：面板出现、计数正确、phase 折叠、"显示其余"分页、展开时 HTTP 拉行、HTTP 之后的 delta 恰好不再
    触发 HTTP（断言 HTTP 次数 = 1）、`rows_omitted` 触发一次 `since` 补拉、版本缺口触发 HTTP、终态展示结果与日志、`result_unavailable` 时只显示 header
    与行（条目不消失）、queued 行渲染为非按钮（§7.3 的标记规则，PR-12 就有）、切换 session 后回来状态保留（展开态在、每个展开的 workflow
    **恰好一次** HTTP 重拉，`ensureRows` 与重订阅的 full 帧不重复）、内容未变的 WS 重连不重拉行、`/new` 后旧 workflow 从面板消失（`workflow_set`）、
    自动展开也拉行且不写 sessionStorage、WS 断开（5s 轮询）期间折叠的 workflow 只发 `rows=none`、**suspended 订阅升级**（在 session 无进程时
    订阅 → mock 推 sessions_update 且快照 `protocol` 非空、state 仍为 `ready` → 客户端重订阅并收到 `workflow_state`）、`body.kbd-open` 时面板隐藏、
    §7.7 无障碍断言（含非当前 session 的终态不播报、切过去也不播报）。
  - **§11.4b drill-in（PR-13）**：点行进入 drill-in（一次点击只发一次 `agent_events` 请求、`switchTo` 只调一次且带 crumb）、Esc 返回、202 pending
    后自动进入、WS `pending` 回退 3s poll、attempt 徽标展开后可进入历史 attempt。
- 截图：`locator.screenshot()` 经 `NODE_PATH=test/e2e/node_modules`（同 `take-screenshots.js`），
  light/dark × desktop / iPhone 13；附在 PR。

### 11.5 闸门

`gofmt -l` 为空；`go vet ./...`；**`GOOS=windows go vet ./...`**（必过的 `build-windows` job 的本地等价，`ci.yml:436-457`；FIFO 测试须在 `*_unix_test.go` + `//go:build !windows`，§10）；`GOTOOLCHAIN=go1.26.6 make lint-staticcheck`；
`lint-server-handlers -mode fail`（含 `hubOptionsFieldBaseline`）；**`go test ./internal/session -run TestRouterBudget`**
（Router 字段 18 / 方法 98 / 类型引用 5，均为 ratchet 台账指标；本特性的 sweeper、resolver、R3 都不得新增 Router 方法或 `*Router`
参数）；`node scripts/check-enum-literals.mjs` 与其 test；`node --test scripts/check-ws-receivers.test.mjs`；js-ratchet `--check`；
**`go run ./tools/ratchet-raises -base origin/master`**（必过 job 的本地等价）；`check-ws-contract`；
`check-mock-rest`；`node --test scripts/workflow-state.test.mjs`；CLAUDE.md 模块清单测试；推送前全仓
`go test ./...`。

## 12. 发布与兼容

- **无 config 开关**：纯观测功能，无副作用；CC 不发相应字段时自然不显示（L4 降级），开关只会增加
  一条永远打开的分支。唯一改变既有可见行为的是 §9，单独 PR、可单独 revert。
- **向后兼容**：EventEntry 只 additive omitempty（不 bump SchemaVersion）；SessionSnapshot 新键
  omitempty；sessions.json 新键旧版忽略；新 WS 帧旧前端无注册——但同仓发布，`check-ws-contract` 保证
  前后端同 PR。Workflow tool_use 的 Detail 文本变短（不再含脚本前缀），属有意修正。
- **CC 版本**：已验 2.1.288；更老的 CC 没有 Workflow 工具（无影响）；更新的 CC 改形态走 §5.7 降级，
  expvar 计数可观测。
- **回滚**：各 PR 独立可 revert；revert Tracker 接线即回到今天行为。

## 13. 风险

| # | 风险 | 缓解 |
|---|---|---|
| R1 | `workflow_progress` 是 CC 私有 SDK 形态，无稳定承诺 | 类型错容错 + 分级降级 + 计数；fixture 随 CC 升级刷新 |
| R2 | readLoop 单 goroutine 的每快照成本：解码 ~1.9ms（含 envelope，比今天多 ~0.25ms）**加上** Observe 的规范化与行重建、board wake 的逐行 diff / 盖 Rev 副本 / `byAgentID` / Summary——都同步跑在 readLoop 上；不加记忆化时含 `=` 的 lastToolSummary 让 2000 行快照的 redact 达 ~15ms | slim typed、无包装类型；规范化按原串 maphash 记忆、`byAgentID` 在 id 集合不变时复用、CoW 共享切片跳过 diff（§4.3、§5.8）；`BenchmarkReadEvent_*` 与 `BenchmarkObserve_*` 两组绝对预算（§11.2）；不在 session/server 层二次解析；replay 三遍有前缀门控（§5.9）。若实测稳态仍超预算，退路是让 wake 只发信号、由 board 自己的 goroutine 合并（增加一个 goroutine 生命周期，故不作首选） |
| R3 | Board / Hub 新增并发路径；board 与 session 表锁之间的死锁 | 单写者 Tracker + 唤醒式回调 + board mutex 版本守卫 + immutable 发布；锁序 表锁 → b.mu → Tracker.mu、持 b.mu 不取表锁、通知走 timer goroutine、读者无锁（§5.8）；事务内 bind 测试 + race 测试 |
| R4 | 重启时 replay 被淘汰导致 taskId↔runId 丢失 | Ref 持久化 + 按 agentId 磁盘定位 |
| R5 | 慢 tab 带宽 / 内存 | header-only 初始帧 + delta + 1Hz 限速 + 队列深度 > 8 跳过 + 每帧 ≤ 192KiB + HTTP gzip 拉行 |
| R6 | 保活改动让 session 永不过期 | `workflowPinMax=6h` 无观测后放开；scratch 按观测时间老化；驱逐有回退 |
| R7 | shim idle timer 不随 stdout 刷新，naozhi 断开 > 4h 仍杀 CLI | 既有行为，记录；另开 issue |
| R8 | js-ratchet / 前端闸门多 | 预先申请覆盖全特性的 ratchet-raise issue；逐 PR 台账；DOM API 构建零新增 sink |
| R9 | hook / control_response 改为行首锚定后，CC 改键序时快路径失效 | 结构性兜底已存在（`protocol_claude.go:475`）且 control_response 兜底改走 `parseControlAck`；fixture 断言前缀形态 |
| R10 | 修 tailer allowedRoot 改变既有行为（更多 tailer 真正起来，触及 50 上限） | 50 上限与 capacity 降级本已存在；PR 描述记录 |
| R11 | CC 状态 / 词表再变（新 state、新 status） | 规范化表单点实现 + unknown 透传 + 手写 fixture 随升级刷新 |
| R12 | 计数型 sessions_update（`BumpVersion`，推进 `stats.version`）让每个 tab 每 30s 重拉一次 `/api/sessions`、整块重绘 sidebar 并重跑 main-state reconcile 等 applied hooks | 仅限有 running workflow 的 session、每 session 至多 1/30s；trailing edge 合并；不置 dirty、不写 sessions.json；代价写明（§5.8） |
| R13 | naozhi 与 shim 断联期间 workflow 显示为"可能过时"（snapshot_stale），而不是立即终态 | 有意取舍：假终态（播报、agent 置 stopped、写进 Ref 再翻回）更糟；R4 在 90s 无进程后收敛为 interrupted，重接或结果文件更早收敛；面板对 snapshot_stale 显示提示 chip（§7.3） |
| R14 | 超大 workflow（约 4k-7k agents，CJK 文本更早）的全量快照行顶到行上限（§1.2.3）：naozhi 侧跳过该行，shim 侧原始行 > 10MB 时 `readStdout` 永久退出、CLI 阻塞在 stdout 上、session 卡死 | naozhi 侧：oversize 分支窥视前缀，置 `Degraded=snapshot_dropped` 并保留旧行，面板不再假装正常（§5.7 L1''）；shim 侧是与 workflow 无关的既有行为（任何超大行都会触发），另开 issue（`ErrTooLong` 时跳过该行而不是结束循环），本 RFC 不改；实测最大 398 agents，离上限一个数量级 |
| R15 | RunDir 解析与后台读取改为异步后，首次展开 / drill-in 可能短暂看到 pending（202）或 `result_unavailable` | 解析是一次 EvalSymlinks + 少量 Lstat，正常 FS 上毫秒级；客户端已有 202 的有界重试；换来的是 readLoop、b.mu 与表事务里没有任何系统调用（§5.8），挂死的 FS 最多卡住 8 个后台 goroutine |

## 14. 实施计划

按序号合并；同一行的"依赖"为空即可与前序并行。**任何**改动 `static/*.js`（含 `go run ./tools/gen-contract` 重生成的 `contract.js`）或
`test/e2e/golden/pins.json` 所列 golden 的 PR，必须在本 PR 内手改 js-ratchet 基线并追加 `scripts/ratchet-raises.jsonl` 行（引用 **PR-10 之前**
开好的、带 `ratchet-raise-approved` 标签的特性 issue），并在 PR 描述列出实际 gate 的 from/to。台账键按 `tools/ratchet-raises/metrics.go:244-287`：
行数只有 `js-ratchet:TOTAL.lines`（per-file `lines` 只改基线、不进台账）；per-file 键只有 `maxFnLines` / `fnOver100`；pin 是 `golden:<file>`，
`routes.golden.json` 不是 pin（v4 更正，§7.1）。

### PR-1 fix(cli): hook/control_response 快速跳过改为行首锚定

- 范围：§5.1(3)。
- 文件：`internal/cli/protocol_claude.go`（:451、:454、:478 兜底）、`internal/cli/protocol_claude_test.go`。
- 测试：`"label":"hook_tests"` 快照帧不再被丢；assistant tool_use `input` 含 `{"subtype":"hook_started"}` /
  `{"type":"control_response"}` 的帧照常交付（旧代码失败）；真实 hook / control_response 帧仍走快路径；
  键序不同的 control_response 经兜底仍得到 ack。
- 验收：现有 hook skip 测试全绿；新回归测试在 master 上失败、在本 PR 上通过。
- 依赖：无。

### PR-2 fix(cli,session): local_workflow 不走 SubagentLinker

- 范围：§5.10 第一条（PR-7 之前的部分：task_start 的 TaskType + 遗留 id 形态启发式）。
- 文件：`internal/cli/process_readloop.go`（:663-665）、`internal/session/router_shim.go`（:433）、
  `internal/cli/process_event_query.go`（:75-82）及测试。
- 测试：三条路径对 `local_workflow` 不调用 Resolve / DispatchResolve（fake linker 计数）；InjectHistory 中
  task_start 不在批内、id 为 `w`+8 的孤儿 progress 不 Resolve；`a…` 孤儿照常 Resolve。
- 验收：含 > 500 条 progress 的历史 session InjectHistory 后无 Resolve 日志、无 tombstone；local_bash 行为
  不变。
- 依赖：无。

### PR-3 fix(cli,session): 后台 task 帧不让重连误判 midTurn

- 范围：§5.10 第二条（中性帧前缀识别不解码、`unknown` 判定、**在 `startReadLoop` 之前**经注入的 resolver 读 JSONL 尾裁决）。
- 文件：`internal/cli/wrapper.go`（:586 `SpawnReconnect` 增 `resolveUnknown func(helloSessionID string) (idle bool)` 参数、
  :625-641 在 `applyReconnectVerdict` 之前调用；:673-720 `reconnectVerdict` / 中性帧）、`adopted_turn.go`（verdict 类型可表达 unknown，
  但交给 `applyReconnectVerdict` 之前已被折成 idle / midTurn）、`internal/session/router_shim.go`（:365-370 在调用 `SpawnReconnect` 前用
  `sess.Workspace()` + `state.SessionID` 构造 resolver 闭包，路径同 `markTranscript` 的 `backendProfile(...).ResumeTarget`；**不新增 Router
  方法**；JSONL 尾窗按 §5.10 读：`OpenRegular(path, 0)` + `ReadAt` 尾部 64KiB、必要时放大到 1MiB，**不**把 64KiB 当 maxBytes）、新
  `internal/osutil/open_regular_unix.go` / `open_regular_nonunix.go` + `open_regular_unix_test.go`（`//go:build !windows`，FIFO 用例在这里，§10；本 PR
  首个使用者，PR-9 / PR-13 复用）、`process_reconnect_drain_bug_test.go`、`adopted_turn_test.go`。
- 测试：见 §11.2 reconnectVerdict 行，含"live result 紧随 startReadLoop 到达"、5MB JSONL fixture、末条记录 200KiB。
- 验收：空闲 + 后台 workflow 的 session 重启后为 Ready；前台长 Bash + 后台 workflow 洪水（ring 已绕回）
  重启后仍为 Running；`reconnectVerdict` 不解码中性帧（计数）；`go test ./internal/session -run TestRouterBudget` 不变；`GOOS=windows go vet ./...` 绿。
- 依赖：无。

### PR-4 fix(server): agent tailer 的 allowedRoot 用共享 ProjectsRoot

- 范围：§8.3 前置修复、§8.1 唯一根（server 侧）；`IsValidWorkflowRunID` 与 runID 正则（v4 从 PR-9 前移：PR-8 的 R0 复核 Ref.RunID 就要用，
  而 PR-9 依赖 PR-8）。
- 文件：新导出 `internal/claudefs` 的 `ResolvedProjectsRoot`、`internal/claudefs/path.go` 的 `IsValidWorkflowRunID` + test；
  `internal/dashboard/ext/agentevents/handler.go`
  （`Deps.ProjectsRoot` 新字段，`New` 优先用它、为空回落；`claudeProjectsAllowedRoot` 改调 claudefs helper；`jsonlPathUnderAllowedRoot`
  的最终比较改用 `osutil.PathContainedInRoot(resolved, root)`，候选在前）；
  `internal/server/build_server.go`（:66 的 claudeDir 求一次 root，:192 传给 `agentevents.New`）；`build_dashboard.go`（:113
  **`HubOptions.AllowedRoot` 改填 projects root**——它在 server 包里只被 `wshub.go:156` 读，不加字段，`hubOptionsFieldBaseline = 16`）；
  `agent_tailer_registry.go`（:49、:186-191 改用 `osutil.PathContainedInRoot`）、`agent_tailer_pathcheck.go`；
  `wshub_wired_linkers_test.go`（:89-92 断言值改为 projects root）；测试。
- 测试：生产配置（workspace ≠ projects root）下 ensureTailer 接受 transcript、拒绝 projects 外路径；
  非对称 symlink 仍拒（#1533 用例保留）；agentevents 与 tailer 拿到同一个 root；`IsValidWorkflowRunID` 的正反例（`..`、`/`、`wf_`、超长）；
  darwin 上 root 与路径大小写不同时 agentevents 与 tailer 判定一致。
- 验收：本地 Agent drill-in 走 WS 实时而非 3s poll（devtools 可见 agent_event 帧）；`lint-server-handlers -mode fail` 不抬基线。
- 依赖：无。

### PR-5 feat(clievent,cli): 解码 workflow 字段 + golden fixture

- 范围：§4.1、§5.1(1)(2)(4)；`ReadEventInto` 的类型错容错；`process_event_format.go` 只修
  `task_updated`/`task_notification` 的 Summary 与 Status（不做 §9 的丢弃）；`deliverEvent` 前清空大字段、
  `readEventBuf` 经 `handleShimStdout` 的 `defer` 清空（覆盖 kill 时的提前 return；此时无消费者，先保证不泄漏）；
  身份字段类型错按 Failed、Partial 身份复核、出错路径上的第二遍遮蔽解码（§4.1.1 规则 3-5）；Workflow tool_use Detail；linker replay walk
  只解 `task_started` 前缀行。
- 文件：`clievent/event.go`、新 `clievent/workflow.go` + test、`clievent/tool_input.go` + test、
  `protocol_claude.go`、`process_readloop.go`（deliverEvent 前清空 + defer 清 buf）、`process_event_format.go`、
  `process_extra_test.go`（:1395-1440）、`internal/session/router_shim.go`（:416-450 前缀门控）、
  `internal/cli/testdata/`（fixture；PR-6 移入 `internal/cli/workflow/testdata/`）。
- 测试：§11.2 解码 / hook 之外的 `FormatToolInput` / eventCh-readEventBuf 行 + Fuzz + bench。
- 验收：bench 数字（`-count 5`，含跳过路径对照）贴 PR，满足 ≤ 1.5ms / ≤ 400KB；`wsproto.schema.json`
  无变化（Event 不是 wire 类型）。
- 依赖：PR-1（避免 fixture 被误跳过）。

### PR-6 feat(cli/workflow): Tracker 纯逻辑包

- 范围：§4.2（规范化顺序、粘滞 agentId、attempt 历史、状态映射、StartedAt 来源、`IsRunning` / `IsUnsettled` 谓词）、§4.3（先 redact
  后截断，含 workflow 层字符串；按原串 maphash 记忆）、§5.2（锁 / 私有 Version / CoW / 结果文件合并只校验 taskId；**无 `ApplyJournal`**）、
  §5.3 的 Tracker 侧上限（live 非终态 16 带行 / 32 header-only、phase 200）、§5.5、§5.6(1-3,5)、§5.7（含 `IsWorkflowTask`、身份复核、
  `NoteDropped` → `snapshot_dropped`）、`SeedFromReplay`（每 task 每帧类只解最新一行、`Set.SeedWrapped`）、`MergeResultFile` 纯函数（瘦结构的
  `workflowProgress[]` 复用 `clievent.WorkflowItem`，行带 label / model / tokens）、
  `agentEqualIgnoringRev`（供 board 用，放在 workflow 包以便字段数钉测试与 Agent 定义同处）。
- 文件：新 `internal/cli/workflow/{types.go,tracker.go,normalize.go,classify.go,seed.go,merge.go,equal.go}` + tests
  （含 `bigSnapshot(n)` 测试 helper，§11.1）；CLAUDE.md `cli` 行子包列表加 `workflow`。
- 测试：§11.2 Tracker 规范化 / 生命周期 / `IsWorkflowTask` / 结果文件合并 / SeedFromReplay 行（总解码次数 O(task 数)）+ race；
  `BenchmarkObserve_*` 的 Tracker 部分。
- 验收：probe fixture 回放结果与 `wf_*.json` 等价（状态、计数、tokens）；手写 fixture 全部按 §4.2 表
  规范化；无 session/server import。
- 依赖：PR-5。

### PR-7 feat(cli): Process 接入 Tracker

- 范围：`Process.workflows`、`Observe` 接线、`SetOnWorkflowChange`（唤醒式，在 Tracker.mu 外调用）、`Workflows()` 访问器、
  SpawnReconnect 中 `SeedFromReplay`（`startReadLoop` 之前，位于 PR-3 的 resolver 调用旁）、`ev.WorkflowTask` 标记、workflow progress /
  updated / notification 条目打 `TaskType=local_workflow`，InjectHistory 过滤改认它（§5.10）；readLoop oversize 分支窥视前缀并调
  `NoteDropped`（§5.7 L1''）。
- 文件：`internal/cli/process.go`、`process_readloop.go`（:600-610；:189-195 oversize 分支）、`process_event_format.go`（TaskType）、
  `process_event_query.go`、`wrapper.go`（:597-641，`startReadLoop` 在 :641）、测试。
- 测试：用 fake shim 逐行喂 probe → Set 正确；空闲期（无 Send）同样更新；replay 种子 + live 无竞态；
  非 workflow 的带 summary 的 task_progress 不打 TaskType；§11.2 超长行。另开 issue：shim 遇 `ErrTooLong` 结束 stdout 循环（§13 R14）。
- 验收：`go test -race ./internal/cli/...` 绿。
- 依赖：PR-6、PR-3（同改 SpawnReconnect / wrapper.go，避免冲突）。

### PR-8 feat(session): workflow board、版本、快照摘要、Ref 持久化、保活

- 范围：§5.8 全部——**锁序**（表锁 → b.mu → Tracker.mu，持 b.mu 不取表锁）、通知经 notifyTimer goroutine 异步发出
  （structural 闭包 / `BumpVersion` 闭包）、无锁访问器、唤醒式合并 + 版本守卫 + 进程身份、淘汰接管与 board 上限（非终态 16 / 终态 5，
  连带 `last` / 缓存 / 索引）、wire version / 行 rev 重盖（`agentEqualIgnoringRev`）、`rows_gen`、epoch、commit 之后绑定、同 proc bind no-op、
  指针携带（**respawn 经 `respawnSnapshot` 传入 `installFreshSession`、先于绑定**）、Summary 含 version、sessions_update 节流、Ref（含
  `last_observed_at`、从有界集合派生、过期 unknown 不写）、保活四处；board 非终态上限**只裁 retained**；RunDir 解析的"锁内登记 → 锁外执行 →
  复核应用"骨架与有界 I/O 派发（resolver 可注入；本 PR 不接 `ResolveWorkflowRunDir`，RunDir 恒为 ""，PR-9 接上）；§5.6(6a) 的按
  `ProcessEnd` 区分（经 `bookProcessEnd` 回调扇出）；§5.9 R0/R2（R4 / R5 的 sweeper 与 R0 终态补行在 PR-9）。
- 文件：新 `internal/session/managed_workflow.go` + test、`managed.go`（字段）、`managed_query.go`（:207 附近）、
  `managed_cost_end.go`（`bookProcessEnd` 的 `SetOnEnd` 回调扇出到 board）、
  `sessionview/snapshot.go`、`store.go`、`respawn_snapshot.go`（`respawnSnapshot.workflows`：`snapshotRespawn` 取、`rereadSameEntry` 重读）、
  `router_lifecycle.go`（:665 调用处传 `snap.workflows`；:722 `installFreshSession` 增 `board *workflowBoard` 参数，在 :797-798 的 book* 之前
  `s.workflows = board`，nil 才新建；**不**在 :671 赋值）、`router_shim.go`（:489）、
  `router_rename.go`（:78 携带、:117 绑定）、`router_restore.go`、`managed_release.go`（:20）、`router_cleanup.go`
  （:278-305）、`scratch.go`（:225）、`router_capacity.go`（`evictOldest` 两遍）、
  `internal/server/sessions_shape_test.go`、`internal/dashboard/session/testdata/rest.schema.json`（重生成）。
- 测试：§11.2 Board 并发 / 锁序（含 fake resolver 零次同步调用）、Board 版本、Board 进程结束、Board 容量（含 live > 16）、Board 其他
  （含 respawn 后绑定的是携带来的指针）、保活行；`BenchmarkObserve_*` 的 board 部分。
- 验收：`/api/sessions` 出现 `workflows` 摘要（snake_case 键、含 epoch/version）；sessions_update 次数满足
  "结构立即 + 计数 ≤ 1/30s"，且计数型更新在 WS 连接时也让前端越过 `sessionsUnchanged`（gen 前进、dirty 不置位）；事务内 bind 测试不死锁；
  kill shim 连接（CLI 存活）时 workflow 保持 running + snapshot_stale、重接后恢复 live；**naozhi 运行中杀掉 shim 进程**（`shimOutlivedSocket`
  为假，`process_end.go:86-96`）→ 立即 interrupted（§5.6(6a)，不经 R4）；respawn（如 TTL 回收后再发消息）之后 workflow 帧继续到达同一个 board；
  重启（shim 存活）后摘要恢复且 version 不回退；`TestRESTSchema_IsGenerated` 与 `TestRouterBudget` 绿。
- 依赖：PR-7、PR-4（`IsValidWorkflowRunID`）。

### PR-9 feat(claudefs,session): workflow 磁盘定位、对账与 sweeper

- 范围：§8.1 的 `ResolveWorkflowRunDir`（`PathContainedInRoot` 候选在前、`RelUnderRoot`、按 projectsRoot 拼写重拼、解析 rel 做结构检查）与 path helper
  （非法输入返回 ""），接进 PR-8 的异步解析骨架；session 侧 `HistoryIO.projectsRoot`；`osutil.RelUnderRoot` / `OpenRegularIn` / `OpenDirIn`（§10，
  root 锚定）；§5.9 R0 终态补行、R3a/R3b（R3c 已删）、R5；§5.6(4)(6b)（终态后读一次 + saveTicker sweeper，含 R4 / R5 与在途上限：每 board 2、全局 8、
  卡死占槽）；结果文件瘦结构解析（`workflowProgress[]` 用 `clievent.WorkflowItem`）与 board 缓存（不含 resultPreview）、journal 的 agentId → 偏移
  索引（singleflight、inode / 截断重建）、L2 phase 标题回填。
- 文件：`internal/claudefs/path.go`（其余 path helper；`IsValidWorkflowRunID` 已在 PR-4）、新 `internal/claudefs/workflow.go` + test、
  `internal/osutil/pathroot.go`（`RelUnderRoot`，`PathContainedInRoot` 改为其薄包装）、`internal/osutil/open_regular_unix.go` / `_nonunix.go`
  （`OpenRegularIn` / `OpenDirIn`）+ `*_unix_test.go`（`//go:build !windows`）、`usage.go`（共享常量）、
  新 `internal/cli/workflow/disk.go` + test
  （journal 索引 / 结果文件解析，纯函数，入参是已打开的 reader）、`internal/session/history_io.go`（`projectsRoot` 字段）、
  `router_core.go`（`NewRouter` 里求一次 root）、`internal/session/managed_workflow.go`、
  `router_cleanup.go`（`startCleanupLoop` 的 saveTicker 分支加一行自由函数调用 `sweepWorkflowBoards(r.ss, time.Now())`——**不新增 Router 方法**，
  `routerMethodBaseline = 98` 已满）。
- 测试：§11.2 磁盘 / sweeper 行（假时钟，含合法 run dir 被接受、root 祖先被拒、darwin 大小写、FIFO / 目录 FIFO 不阻塞、挂死 FS 下在途 ≤ 8、
  R4 的 90s 判定与翻回）、Board 进程结束行的 R5、Board 其他行的 R0 终态补行；FIFO 用例全在 `*_unix_test.go`。
- 验收：丢失 `task_updated` 的 run 在 ≤ 90s（60s 无观测 + ≤ 30s tick）内由结果文件置终态；续跑期间
  旧结果文件不让 workflow 变终态；杀掉 naozhi 并等 shim ring 绕回后重启，workflow 仍能显示 runId 与终态；**R4**：naozhi 优雅停止
  （Detach，Ref 里留下 running 条目）→ 杀掉 shim → 重启 naozhi，R0 恢复的条目无进程可重接，≤ 120s（90s + 一个 tick）由 R4 置 interrupted
  （v3 的"naozhi 运行中杀 shim"走的是 PR-8 的 (6a)，测不到 R4）；**R5**：同样的重启后 90s 内发一条消息 spawn 新 CLI，恢复的 running 条目
  ≤ 90s + 一个 tick 后变为 unknown、不再钉保活；没有 shim 的重启后展开一个恢复出来的终态 workflow 能看到全部行；在 run dir 里 mkfifo 一个
  journal.jsonl 不卡住 sweeper；`TestRouterBudget` 绿；`GOOS=windows go vet ./...` 绿。
- 依赖：PR-8、PR-4（`claudefs.ResolvedProjectsRoot`）、PR-3（`osutil.OpenRegular`）。

### PR-10 feat(dashboard/ext/workflows): HTTP 端点

- 范围：§6.2、§6.3 HTTP 部分（校验顺序、`server_now`、`rows_gen`、**行模式 `rows=none` / `since=`**、**404 只表示资源不存在**、磁盘失败为 200 +
  `result_unavailable` / `transcript:false`、queued 200、缓存缺失经 board 的 `loadResult` 合入行、per-agent result 统一走 journal 索引）；§8.2 的
  `subagent.ReadFirstLineIDs` 与 `subagent.ReadFirstPrompt`（一次解码返回两个 ID 与 prompt；`prompt` 字段需要，原计划在 PR-13，前移到这里）
  与 board 的首行缓存。
- 文件：新 `internal/dashboard/ext/workflows/{deps.go,routes.go,handler.go,rest_schema_test.go}` +
  `testdata/rest.schema.json` + tests、`internal/subagent/link_scan.go` 旁新导出 `ReadFirstLineIDs` / `ReadFirstPrompt` + test
  （含 > 32KiB 首行、`sessionId` 在 `message` 之后的键序）、`internal/server/handler_set.go`、`build_server.go`（注入 IPLimiter 与 PR-4 的 projects root）、
  `routes.go`、`routes_snapshot_test.go`、`testdata/routes.golden.json`、`static/contract.js`（API 表 +2 行）、
  `scripts/js-ratchet.baseline.json`（`contract.js.lines` 110 → 112，手改）、`scripts/ratchet-raises.jsonl`、
  `test/e2e/check-ws-contract.mjs`（:120 `REST_SCHEMAS` 追加）、`test/e2e/mock-server.js`（两个路由，含三种行模式与 `result_unavailable`）、
  `scripts/check-mock-rest.test.mjs`（schema 列表 + workflow 响应用例，不动 `ROUTES`）。
- ratchet 台账：`js-ratchet:TOTAL.lines`（重生成的 contract.js +2 行；v3 漏了这一行，`js-ratchet --check` 与 `ratchet-raises` 必红）。
- 测试：§11.2 HTTP 行。
- 验收：curl（带 cookie）对 mock / 本地测试实例返回 gzip JSON；未鉴权 401；非法 key 400；连续请求触发 429；`rows=none` 不带行、`since` 只带变化行；
  删掉结果文件后 GET 仍是 200（`result_unavailable`）；`js-ratchet --check` 与 `go run ./tools/ratchet-raises -base origin/master` 绿。
- 依赖：PR-9（结果/日志与 agent 预览需要磁盘读取）；需要预先开好 `ratchet-raise-approved` issue。

### PR-11 feat(wsproto,server,static): workflow_state / workflow_set 帧 + workflowPushLoop + 前端 store

- 范围：§6.1 全部（`workflow_state` + **`workflow_set`** 两种帧、rows_gen 触发 full、两道深度门、客户端 429 / 404 规则、**`version` / `rowsAt` 分离、
  full 帧在 epoch 与 rows_gen 不变时保留行、fetch 在途时的 full、HTTP 响应不倒退、三种拉取模式与 `needsFetch` 去重**）；前端叶子模块
  `workflow_state.js`（客户端状态机，配 node --test；入参带 def 短名 JSDoc 类型，§6.3）+ `workflow_view.js`（两个 `wsm.on` handler，逐字段拼对象、
  不转交 `msg`；导出函数骨架，不渲染），由 `dashboard.js` 的一行副作用 import 加载，handler 从本 PR 起生效；contractjs ENUMS +
  `check-enum-literals.mjs` 扩展（**只限两个 workflow 模块**，§6.3）。
- 文件：`internal/wsproto/{wsproto.go,registry.go,wsproto.schema.json}`、`internal/contractjs/contractjs.go`、
  `static/contract.js`、新 `internal/server/wshub_workflow.go` + test、`wshub_subscribe.go`（有进程分支启动
  workflowPushLoop、`clientWG.Add(2)`）、新 `static/workflow_state.js`、新 `static/workflow_view.js`、
  新 `scripts/workflow-state.test.mjs`、`.github/workflows/ci.yml`（node --test 列表）、
  `scripts/check-enum-literals.mjs` + `check-enum-literals.test.mjs`（表键恰等、两文件内字面量禁令、其他文件里的同名字面量不报的夹具）、
  `scripts/check-ws-receivers.test.mjs`（R6 夹具：转交 `msg` 给 import 进来的函数被拒、逐字段对象通过）、`static/dashboard.js`
  （`import './workflow_view.js';` 一行）、`static_assets.go`、`routes.go`、`testdata/routes.golden.json`、
  js-ratchet baseline（两个新文件的行、`contract.js` 与 `dashboard.js` 的行数）、`scripts/ratchet-raises.jsonl`、CLAUDE.md:242。
- ratchet 台账：`js-ratchet:TOTAL.lines`；新文件若含 > 100 行的函数则 `js-ratchet:TOTAL.fnOver100` / `js-ratchet:MAX.maxFnLines`。per-file 行数只改基线；
  `routes.golden.json` 不是 pin（v3 列的 `golden:routes` 删除）。
- 测试：§11.2 wsproto / Hub / 前端 `workflow_state.js` 行；`check-ws-contract`（嵌套检查的 `typedFns` 增加、改一个 `WireView` 的 json tag 即报错）、
  `check-ws-receivers`（含新夹具）、
  `check-enum-literals`（含新夹具；在现有 static 文件上首跑即绿）。
- 验收：浏览器 devtools 见 `workflow_set` → `workflow_state` header-only full → delta 序列；`/new` 后收到空的 `workflow_set`；
  慢 client 模拟下（含队列满时三个 workflow 同时终态）不触发 `wsDropThreshold`；**CLI 退出**（cli_exited）且 eventPushLoop 阻塞在
  resubscribe 期间 interrupted 帧照常送达；**只 kill shim 连接**（CLI 存活）时收到的是 `degraded:"snapshot_stale"` 的 running 帧，
  **不**出现 interrupted，重接后恢复。
- 依赖：PR-8；需要预先开好 `ratchet-raise-approved` issue。

### PR-12 feat(static): workflow 面板 UI

- 范围：§7.2-7.5、§7.7；`<details>` 的 toggle 监听 + `ensureRows`（自动展开同路径、只持久化用户操作）、`onSessionsRefreshed` 兜底
  （经 `onSessionsApplied`，跳过 remote node，以 Summary 裁剪 store，按行模式取 `rows=none` / `since`）、切走 session 时释放行、elapsed 的 offset 也取自 HTTP、
  `.sr-only` 状态标签、独立的 `wf-*` CSS 规则 + `data-agent-id`、degraded chip（含 `snapshot_dropped`）、`body.kbd-open` 时整个面板隐藏、
  终态播报只限当前 sid；suspended 订阅在快照 `protocol` 非空时升级（§6.1）。
- 文件：`static/workflow_view.js`、`dashboard.js`（PR-11 的副作用 import 改为具名 import、renderMainShell +2 行、registerActions 表项、:346-347 旁
  `onSessionsApplied(onSessionsRefreshed)` 一行、session 切换处调 `onWorkflowSessionSwitched` 一行；全特性合计约 +7，余量 18，§7.2）、
  `session_list.js`（`sessions_update` 处理器里 suspended 升级，几行）、`running_banner.js`（toolVerbs）、
  `css/views.css`（`.wf-*`）、`css/responsive.css`（kbd-open）、e2e spec（含 §7.7 断言）、mock-server（64-bit 长度分支 +
  ws override，含 `workflow_set`；sessions_update 带 `protocol` 的快照）、截图、`scripts/ratchet-raises.jsonl`、js-ratchet baseline。
- ratchet 台账：`js-ratchet:TOTAL.lines`，以及可能的 `js-ratchet:<file>.maxFnLines` / `fnOver100`（per-file 行数只改基线）。
- 测试：§11.4a。
- 验收：desktop/mobile × light/dark 截图；三个 running workflow 时 transcript 仍可见；400-agent 合成
  fixture 下滚动与更新流畅（Performance 面板单帧 < 16ms 的截图附 PR）；HTTP 拉取次数断言；WS 连接时计数型 sessions_update 后
  兜底刷新确实执行（断言 hook 被调用）；多节点 mock 下 remote session 不触发 workflow HTTP。
- 依赖：PR-10、PR-11。

### PR-13 feat: workflow agent drill-in

- 范围：§8.2-8.4。
- 文件：`internal/dashboard/ext/agentevents/handler.go`（board 查找挪到 :142 之前；RunDir 未就绪 → 202；transcript 经 root 锚定的
  `osutil.OpenRegularIn` 打开并以同一 fd 分页）、`deps.go`（`WorkflowAgent`）、`internal/subagent/transcript.go`（`NewTranscriptReaderFrom(f, open)`；
  `openOrReuse` / `reprobeRotation` 的每次重新打开经注入的 opener，旋转探测改 `os.Lstat`；`NewTranscriptReader(path)` 默认 opener 为
  `OpenRegular(path, 0)`，既有 Agent drill-in / tailer 一并受保护）+ `transcript_fifo_unix_test.go`（`//go:build !windows`）、
  `internal/server/wshub_agent.go`（:130 之前）、`agent_tailer.go` / `agent_tailer_registry.go`（doneFn；`ensureTailer` 增 opener 参数、用
  `NewTranscriptReaderFrom`）、
  `internal/session/managed_workflow.go`（`WorkflowAgent`）、`static/agent_view.js`（switchTo 第二参；`pending` 分支回退 3s poll）、
  `static/workflow_view.js`（attempt 列表）、e2e、`scripts/ratchet-raises.jsonl`、js-ratchet baseline。
  （`ReadFirstLineIDs` 已在 PR-10 落地。）
- ratchet 台账：`js-ratchet:TOTAL.lines`（`agent_view.js` / `workflow_view.js` 的 per-file 行数只改基线）。
- 测试：§11.2 drill-in 行（含 tail 中途换 FIFO 不阻塞、`Close()` 返回、其他 tailer 照常推进）；§11.4b。
- 验收：点击 running agent 实时看到 transcript 增长（一次点击一次 `switchTo`）；新启动的 agent（文件未落盘）先 202 后自动进入；
  done 时收到 agent_done；queued 不可点；重启后无进程的 session 仍可 drill 已完成 agent；tail 中把 agent jsonl 换成 FIFO 后其他 agent 的实时
  transcript 不停；`GOOS=windows go vet ./...` 绿。
- 依赖：PR-4、PR-10（`ReadFirstLineIDs`）、PR-12。

### PR-14 perf(cli,static): workflow 进度不再进 event ring

- 范围：§9。
- 文件：`internal/cli/process_event_format.go`、`process_extra_test.go`、`static/running_banner.js`（timer 修复）、
  相关 e2e golden（若受影响按 pins 流程重录）、`scripts/ratchet-raises.jsonl`（若 `running_banner.js` 行数
  或任一 golden pin 变化）。
- ratchet 台账：每个变化的 `golden:*` pin 一行（任何 pin 变化都算抬升，`metrics.go:376-401`）；
  `running_banner.js` 若净增行数则 `js-ratchet:TOTAL.lines`。
- 测试：workflow task_progress / task_updated 不产生 entry；非 workflow（mcp_task 带 summary）照旧；
  task_start / task_done 保留且 Summary 正确；Cleanup 仍不过期 running workflow session；前端空闲 progress
  不再启动 turn timer。
- 验收：跑一个 50-agent workflow 后 ring / 持久化 log 中无 workflow progress 行，首页对话气泡完整。
- 依赖：PR-8（保活已改看 board）、PR-12（用户已有替代视图）。

### PR-15（可选）feat(static,session): sidebar workflow 徽标 + activity 回落

- 范围：非当前 session 的卡片从 `s.workflows` 显示 "⚙ 5/8"（计数至多 30s 陈旧，依赖 PR-8 的 `BumpVersion` 型 sessions_update——
  它推进 `stats.version`，WS 连接时 `renderSidebar` 才会重跑）；`node` 非 local 的卡片不显示（NG3）；Q12 的 `LastActivity` 回落。
- 文件：`static/session_list.js`、`internal/session/managed_query.go`（LastActivity 回落）、
  `test/e2e/golden/sidebar.json` + pins、`scripts/ratchet-raises.jsonl`。
- ratchet 台账：`js-ratchet:TOTAL.lines`、`golden:sidebar.json`（`session_list.js` 的 per-file 行数只改基线）。
- 测试：WS 连接状态下徽标在计数变化后 ≤ 30s（假时钟 / mock 的 sessions_update，`stats.version` 前进）刷新；终态立即刷新；
  remote session 不显示徽标。
- 依赖：PR-8。

## 15. 开放问题

| # | 问题 | 建议 |
|---|---|---|
| Q1 | 是否在内存里保留 `promptPreview/resultPreview`（截 200 runes）以便 queued agent 也能看到 prompt？ | **不保留**。解码成本与内存是主要开销（§1.2.3）；done/running agent 的 prompt/result 按需读磁盘（终态结果有缓存）；queued 只显示 label。若用户反馈需要再加 `promptPreview` 单字段 |
| Q2 | workflow 索引持久化到 sessions.json 还是 event log？ | **sessions.json**（`code_changes` 先例，`store.go:62-64`）：体积小、随 session 生命周期、restore 路径已存在；event log 会被 rotation 淘汰 |
| Q3 | §9 是"丢弃"还是"节流"（比如每 phase 变化留一条）？ | **丢弃** progress/updated，保留 start/done。面板已是唯一真相源；节流仍会在 400-agent 下产生 ~800 条 |
| Q4 | running workflow 对 idle TTL 的钉住上限 | **6h 无观测后放开**（常量）。CC 每次 agent tool call 都有 progress，6h 全无观测基本等价于挂死；不引入配置 |
| Q5 | drill-in 用新参数 `agent_id` 还是复用 `task_id`？ | **复用 `task_id`**，board 优先查找。agentId 形态天然通过现有正则，免改 `node.ClientMsg`、contract 与前端调用面 |
| Q6 | 重试的旧 attempt transcript 是否可看？ | **v2 做**：builder 记录每个 index 的 `PrevAgentIDs`（≤8），`byAgentID` 为其建索引，attempt 徽标展开可点（§4.2、§8.4）；重启后若快照里已无旧 id，可从 journal 的多条 `started`（同 key 不同 agentId）补，列为后续 |
| Q7 | remote node 上的 workflow | **v1 不做**（NG3）。需要 reverseconn 与 node RPC 扩展，单独 RFC |
| Q8 | ~~linker 用 `DefaultDir()`、history 用配置 claudeDir 的不一致~~ | **已撤销**：两者是同一值（`main.go:165-167`），见 §8.1；所有路径共用一个 ProjectsRoot |
| Q9 | resume（`resumeFromRunId`：同 runId、新 task_id）如何展示？ | 视为新 workflow；前端按 `run_id` 把旧条目折叠为"已续跑"子行；后端不合并，但结果文件按 taskId 归属（§5.5）。adopt 的 `paused` 占位沿用同一 task_id，不产生新条目 |
| Q10 | 面板是否默认展开？ | 桌面只自动展开最新的 running、其余折叠；移动端一律折叠；面板整体限高；记住每个 workflow 的展开状态于 sessionStorage |
| Q11 | 终态 workflow 在面板上保留多久？ | 每 Process 的 Tracker 5 个；**每 board 5 个**（live 与 retained 合并 LRU，带行与结果缓存）；快照摘要与面板 3 个（running 全部 + 最近 3 个终态）；Ref = board 的有界集合（≤ 16 非终态 + 5 终态）。v2 的"更早的 N 个"链接删除：§6.2 没有列表端点，Summary 也不带更早的 task_id，客户端无从知道它们；为 2 个额外条目加一条路由（golden、contract、REST schema、mock）不划算。若有需求，后续加 `GET /api/sessions/workflows?key=` 返回 Ref 级行 |
| Q12 | 是否在 sidebar activity（`last_activity`）里显示 workflow 进度替代被 §9 移除的 phase label？ | **是，低优先级**：snapshot 在 parent 非 running 且有 running workflow 时，`LastActivity` 回落为 "Workflow <name> · done/total"，随 PR-15；与徽标同一刷新机制（sessions_update 节流，至多 30s 陈旧） |
