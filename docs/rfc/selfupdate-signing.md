# RFC: selfupdate 加密签名验证

> **状态**: Draft v1（待评审）
> **作者**: naozhi team (cron-cr)
> **创建**: 2026-06-04
> **范围**: 给 selfupdate 升级链加一层真正的发布者身份签名验证，使下载的二进制可信不再单纯依赖"checksums.txt 与 binary 同源同 token"假设
> **关联 issue**: #1738
> **关联代码**:
> - `internal/selfupdate/selfupdate.go`（`Download` :152-179；`verifyChecksum` :567-626；`verifyPinnedChecksumsFile` :189-212；`fetchFile` :474-565；TOFU pin 锚点 :112-151）
> - `internal/selfupdate/checker.go`（`doInstall` :236-317 调用 `Download`→`Replace`）
> - `cmd/naozhi/upgrade.go`（:37 `LatestRelease`、:76 `Download`、:84 `Replace`）
> - `.github/workflows/release.yml`（:54-62 build+sha256；:83-100 checksums.txt + release 发布）
> - `install.sh`（:195-212 shell 端 checksum 校验）
> - 现有测试：`internal/selfupdate/selfupdate_pin_test.go`、`selfupdate_test.go`、`verify_checksum_dup_test.go`

---

## 1. Background & problem

### 现状（事实）

升级链当前的"完整性"保证全部建立在 **SHA-256 同源校验** 上：

1. `release.yml` 在每个 matrix cell 用 `sha256sum "${OUTPUT}" > "${OUTPUT}.sha256"`（:62），release job 再 `cat dist/*.sha256 > dist/checksums.txt`（:84），作为 release asset 上传（:99）。
2. 客户端 `Download`（selfupdate.go:152-179）先 `fetchFile` 拉二进制、再拉 `checksums.txt`，然后：
   - `verifyPinnedChecksumsFile`（可选 TOFU pin，:189-212）
   - `verifyChecksum`（:567-626）从 `checksums.txt` 取该 asset 的 hash，对下载的二进制做 SHA-256 比对，`subtle.ConstantTimeCompare`。
3. `fetchFile`（:474-565）已经做了相当硬的传输层加固：https-only（:481/:497）、redirect 主机锁 github.com/githubusercontent.com（:519-530）、DNS-rebinding/SSRF 防护（`blockPrivateDialContext` :412-449，dial 已校验 IP :447）。

### 为什么这仍是问题

`checksums.txt` 与二进制 **走同一条路径、由同一个 GitHub release 写权限产生**。代码自己在注释里点明了威胁模型缺口（selfupdate.go:113-128、:382-397）：

> "a leaked GitHub token lets an attacker swap BOTH files in lock-step, so the fetched checksums.txt is no stronger than the leaked-token threat model."

即：SHA-256 校验只防 **传输中篡改**（已被 https+host-pin 大幅覆盖），**不防发布源被攻陷**（泄露的 `contents: write` token、被攻陷的 release workflow、恶意 maintainer push tag）。一旦攻击者能写 release，他同时替换 `naozhi-linux-arm64` 和 `checksums.txt`，客户端会 **顺利校验通过并 chmod 0755 执行**（:175 → `Replace` :233 → `RestartServiceNoWait`）。这是远程代码执行级别的供应链风险，且 `ModeAuto` 下全自动、无人工确认（checker.go:227 / :307）。

现有的 `NAOZHI_UPGRADE_PIN_SHA256`（selfupdate.go:129）是一个 **TOFU stopgap**：要求运维带外记录一次 `checksums.txt` 的 hash 并设进环境变量。它把"换 release"提升到"还要攻陷运维 pin 存储"，但代价是每次发版运维都得手动更新 pin，几乎没人长期坚持，而且默认 unset（:191 返回 nil，行为不变）。注释明确写着 "a fully-signed release flow (cosign / Sigstore) is the proper long-term fix and is tracked separately under the same issue."（:126-128）——本 RFC 就是那个 long-term fix。

### 可复现症状

无法"复现 bug"（不是缺陷），但缺口可演示：在一个测试 release 里把二进制和 `checksums.txt` 一并替换为任意内容，`Download` 不会拒绝（仅 `verifyChecksum` 比对二者一致即通过）。`selfupdate_test.go:295 TestDownload_OK` 正是用自洽的 binary+checksums 构造，证明只要二者一致就放行。

---

## 2. Goals & non-goals

### Goals

- G1：客户端在 `chmod 0755` / `Replace` **之前**，验证下载产物携带的 **由 naozhi 发布者私钥产生的签名**，公钥编译进二进制（embedded），离线即可验证。
- G2：发布流水线（release.yml）新增签名步骤，对发布产物（至少 `checksums.txt`，见 §3 决策）生成签名 asset。
- G3：签名验证 **默认开启且 hard-fail**（验证失败拒绝升级），但提供受控的过渡期开关（见 §8 rollout）。
- G4：支持 **公钥轮换**：客户端内置 **多把可信公钥**（一个 trust set），任一把验证通过即可，使旧客户端在密钥轮换期间仍能验证新签名。
- G5：保留 `NAOZHI_UPGRADE_PIN_SHA256` 作为正交的额外防线（不移除，见 §7）。
- G6：`install.sh`（首次安装路径）的签名验证策略明确（本 RFC 至少给出方向，实现可分期）。

### Non-goals

- NG1：**不引入 TUF / The Update Framework 完整信任根**（root metadata、role 委派、阈值签名）。对一个单仓库、单发布者的项目过度复杂；多公钥 trust set 已覆盖轮换需求。若未来多 maintainer/阈值签名需求出现，再单开 RFC。
- NG2：**不做透明日志 / Rekor 强制查询**（即使选 cosign keyless 也允许 `--insecure-ignore-tlog` 或离线 bundle 验证）——naozhi 升级常发生在受限网络的自托管机器上，强制联网查 Rekor 会把"离线可验证"这条硬需求打破。
- NG3：**不改 SHA-256 校验链**——签名是在其之上叠加，不是替换。`verifyChecksum`、duplicate-entry 防护（:601）、host-pin、DNS-rebind 防护全部保留。
- NG4：**不改 `Replace` / `Rollback` / 备份语义**（selfupdate.go:233-329）——签名只在 `Download` 内、`chmod 0755`（:175）之前插入；on-disk 原子替换不变量零改动。
- NG5：本 RFC **不落地生产代码**（见 §8，`hasLandablePhase1=false`）——需要新签名密钥、流水线改动、公钥嵌入，全部需先评审定方案。

---

## 3. Alternatives considered

需要做两组独立决策：**(A) 签名机制**，**(B) 签名对象**。

### (A) 签名机制

#### A1. cosign keyless（Sigstore OIDC，无长期密钥托管）

- 流水线用 GitHub Actions OIDC 身份向 Sigstore Fulcio 申请短期证书签名，签名+证书+（可选）Rekor 条目打包为 bundle asset。
- 客户端用 cosign 的验证库校验 bundle，约束 OIDC issuer + 仓库 identity（如 `https://github.com/naozhi/naozhi/.github/workflows/release.yml@refs/tags/v*`）。
- **优点**：无需托管长期私钥（最难的运维问题消失）；签名身份天然绑定到"哪个 workflow 在哪个 tag 上跑的"，比"谁有私钥"语义更强。
- **缺点**：
  - 依赖链极重——`github.com/sigstore/cosign` 拉进来一大票传递依赖（go-containerregistry、in-toto、tuf 客户端等），与本项目"不引入外部组件"的设计哲学（MEMORY/DESIGN）冲突最严重。
  - **离线验证脆弱**：keyless 证书短期有效，离线验证需要锁定签名时间 + 内嵌 Fulcio/Rekor 信任根，且 Sigstore 的 TUF 信任根本身会过期轮换；受限网络的自托管 naozhi 机器（升级链的核心场景）容易踩到"信任根过期无法刷新"的坑——这与 NG2/离线硬需求直接抵触。
  - 验证逻辑复杂度高，难以在我们已有的小巧、可单测的 selfupdate 包里保持"几十行可读"。

#### A2. 自管 ed25519（minisign/age 风格，简单，无外部依赖）★ 选中

- 生成一对 ed25519 密钥；私钥存 GitHub Actions secret（或更理想：离线签名后上传），公钥（32 字节）以 hex/base64 常量编译进客户端。
- 流水线对签名对象产生 detached 签名 asset（如 `checksums.txt.sig`），客户端用 **标准库 `crypto/ed25519`** 验证——**零新增依赖**。
- 多公钥 trust set：客户端持 `[]ed25519.PublicKey`，任一验证通过即可，支持轮换。
- **优点**：
  - 仅用 Go 标准库（`crypto/ed25519`、`crypto/sha256`），契合"不引入外部组件"红线，依赖审计成本为零。
  - 离线验证天然成立（公钥内嵌，验证纯本地计算），完美匹配受限网络场景。
  - 验证代码 ~30-50 行，可像现有 `verifyChecksum` 一样彻底单测。
  - 与现有 `subtle.ConstantTimeCompare` / hex 解析风格一致。
- **缺点**：
  - **私钥托管是自己的责任**——泄露即被冒签。缓解：私钥只存 Actions secret 且 release.yml 已经 `check-branch` 限定 master（:18-23）；进阶可离线签名（CI 只打包已签好的 .sig，私钥永不进 CI）。轮换由多公钥 trust set 支撑。
  - 无透明日志，无法事后审计"谁签的"。对单发布者项目可接受。

#### A3. GPG/PGP detached 签名

- 传统 `gpg --detach-sign`，客户端验证。
- **缺点**：Go 没有标准库 OpenPGP（`golang.org/x/crypto/openpgp` 已 deprecated 且 frozen）；GPG 信任模型、子密钥、过期日期复杂度远超需求。劣于 A2。

**选 A2（自管 ed25519）**：唯一同时满足"零新增依赖 + 离线可验证 + 可读可测 + 支持轮换"四条的方案。A1 的 keyless 优势（无私钥托管）很诱人，但其离线脆弱性与重依赖直接撞上 naozhi 的两条硬约束，得不偿失。

### (B) 签名对象

#### B1. 只签 `checksums.txt`（间接签二进制）★ 选中

- 流水线对 `checksums.txt` 生成 `checksums.txt.sig`。客户端先验签 `checksums.txt`，验签通过后照旧用其中的 SHA-256 校验二进制。
- **优点**：单签名覆盖所有平台 asset（一个 .sig 守一个清单）；客户端改动最小——只在 `verifyPinnedChecksumsFile`（:167）之后、`verifyChecksum`（:170）之前插一步 `verifySignature(sumPath, sigPath)`；信任链 `sig → checksums.txt → binary SHA-256` 清晰，且复用全部现有 `verifyChecksum` 加固（duplicate-entry 等）。
- **缺点**：二进制本身不带签名（带外验证者需先拿 checksums.txt）。对我们场景无影响。

#### B2. 签每个二进制 asset

- 每个 `naozhi-os-arch` 配一个 `.sig`。
- **优点**：二进制自包含可验证。
- **缺点**：N 个签名、N 个 asset、客户端要按平台选对 .sig；`checksums.txt` 链路被绕过，反而要重写校验。复杂度高、收益低。

**选 B1**：改动面最小、与现有 checksums 链路天然衔接、单签名覆盖全平台。

---

## 4. Test strategy

签名验证是纯函数、易测，目标覆盖率对齐包现状（≥80%，table-driven，`-race`）。

### 新增 unit 测试（点名，建议放 `internal/selfupdate/signature_test.go`）

- `TestVerifySignature_ValidSig_Accepts`：用测试密钥对（测试内 `ed25519.GenerateKey`）签 `checksums.txt`，注入测试 trust set，断言通过。
- `TestVerifySignature_TamperedPayload_Rejected`：签名后篡改 `checksums.txt` 一字节 → 拒绝。
- `TestVerifySignature_WrongKey_Rejected`：用不在 trust set 的密钥签 → 拒绝。
- `TestVerifySignature_MultiKeyTrustSet_AnyMatchAccepts`：trust set 含 2 把 key，用第二把签 → 通过（轮换语义）。
- `TestVerifySignature_MalformedSig_Rejected`：.sig 内容非法 base64 / 长度错 / 空文件 → 明确错误，不 panic。
- `TestVerifySignature_MissingSigFile_Rejected`：`.sig` 不存在 → hard-fail（防"删掉签名即降级"）。
- `TestVerifySignature_EmptyTrustSet_Rejected`：编译期 trust set 为空时必须拒绝而非放行（防误配成 no-op）。

### 集成测试（扩展现有 `TestDownload_*`）

- `TestDownload_ValidSignature_OK`：httptest 服务额外提供 `checksums.txt.sig`，注入测试 trust set，断言 `Download` 成功且二进制最终 0755。
- `TestDownload_BadSignature_Refused`：服务返回有效 binary+checksums 但 **错误的 .sig** → `Download` 必须返回 error 且 **二进制保持 0600、绝不 chmod 0755**（断言 file mode，因为这是 RCE 防线的关键不变量）。
- `TestDownload_SigFetchFails_Refused`：`.sig` 404 → 拒绝（默认 hard-fail 模式下）。
- 复用现有 `testHTTPTransport`（:472）注入路径，无需联网。

### Regression / 防回归

- 保留并必须继续通过：`selfupdate_pin_test.go`（全部 5 个 `TestVerifyPinnedChecksumsFile_*`）、`selfupdate_test.go:295/337 TestDownload_OK / _ChecksumMismatch`、`verify_checksum_dup_test.go`、`TestReplace_*`、`TestFetchFile_*`、`dial_validated_ip_test.go`。签名是叠加层，这些必须零行为变化。
- 新增 `TestDownload_SignatureBeforeChmod_Order`：用一个会在 chmod 前后留痕的 hook（或检查 mode）证明验签发生在 `os.Chmod(..., 0o755)`（:175）**之前**——这是 §5 的 load-bearing 顺序不变量。
- 流水线侧：在 release.yml 加一个 CI 自检步骤（或 `go test` 内）验证"刚签的 .sig 能被内嵌公钥验过"，防止 CI 用错私钥导致全网客户端拒绝升级（自锁）。

---

## 5. Risk & rollback

### 出错会 break 什么

- **最大风险：自锁（lock-out）**。若 release.yml 用的私钥与客户端内嵌公钥不匹配（轮换失误、secret 配错），**所有已部署客户端将拒绝一切后续升级**。由于 `ModeAuto`/`ModeDownload` 失败会 degrade 成 notice（checker.go:252-264），服务不会崩，但升级通道死锁，只能靠人工 `install.sh` 重装绕过。
  - 缓解：多公钥 trust set（G4）——轮换时新旧公钥共存一两个版本再退役旧的；CI 自检步骤（§4）在发版前就拦住密钥不匹配。
- **传输层不变量（load-bearing）**：`fetchFile` 的 https-only（:481/:497）、host-pin（:526）、`blockPrivateDialContext` dial-validated-IP（:447）必须同样作用于新增的 `.sig` 拉取——`.sig` 必须走 `fetchFile` 同一路径，不能开新的 http 客户端绕过这些防护。
- **on-disk 不变量（load-bearing，绝不能破坏）**：
  - 二进制在验证通过前为 0600，仅 `Download` 在 checksum（且本 RFC 后还要 + 签名）通过后 chmod 0755（:175）。验签必须插在 chmod **之前**，否则窗口期可执行未签名二进制。
  - `Replace` 的 O_EXCL 备份（`copyFileBackup` :668）、原子 rename（:268）、`stagingPattern` 随机后缀（:218）—— **本 RFC 不触碰**，验签全程在 `Download` 内完成。
- **并发不变量**：`latestRelease`（checker.go:114）与 `testHTTPTransport`（:472）是无锁可变包级状态，测试约定不得 `t.Parallel()`。新增的 trust-set 注入点（测试用）必须沿用同一约定——若做成可变包级变量供测试覆盖，**禁止 `t.Parallel()`**，并在注释里写明，复用现有惯例。

### 已有的 pin 测试（点名保护）

`selfupdate_pin_test.go` 的 `TestVerifyPinnedChecksumsFile_{Unset,Match,MismatchRefused,MalformedPinErrors,UppercasePinAccepted}`（:16/:28/:48/:71/:100）锁定 TOFU pin 行为。本 RFC 在 pin 校验**之后**插入签名校验（顺序：pin → 验签 → checksum），这些测试必须继续全绿，证明 pin 语义未被签名层挤掉。

### Rollback

- 代码层：签名验证由 flag/env 门控（见 §8）。线上若签名链出问题，运维设过渡 env 降级到"验签 best-effort warn"或纯 checksum 模式，恢复升级通道，无需回滚二进制。
- 发布层：保留旧 release.yml 不签名也能被"验签可选"阶段的客户端接受，提供平滑过渡。

---

## 6. Observability

- **日志**（沿用 `log/slog`，与 checker.go 一致）：
  - `slog.Info("auto-update: signature verified", "tag", rel.Tag, "key_id", matchedKeyID)`——记录哪把公钥验过（轮换可观测）。
  - `slog.Warn("auto-update: signature verification FAILED", "tag", ..., "err", ...)`——失败路径，配合现有 `c.notify(...)` 给 IM 一条中文告警（仿 checker.go:253 文案）。
  - 过渡期 best-effort 模式下，验签缺失/失败但放行时 emit **一条显著的 SECURITY warn**（仿 `af2a36bb` feishu insecure-webhook 的运行时 SECURITY 告警先例），让运维知道自己在裸奔。
- **metric**：若包内已有 metrics 钩子则加 `selfupdate_signature_verify_total{result=ok|fail|skipped}`；否则本 RFC 不强制新建 metrics 子系统（避免 scope creep），日志 + IM notice 已够。
- **dashboard**：无新增（升级是后台/CLI 行为，不在 dashboard 主流程）。N/A — 升级链无对应 dashboard 面板，加面板属另一范畴。

---

## 7. Compatibility & migration

- **向后兼容**：新客户端能验签；但 **旧 release（无 .sig）** 在 hard-fail 模式下会被新客户端拒绝。因此必须 **先发带 .sig 的 release，再把客户端切到 hard-fail**（见 §8 阶段顺序）。
- **on-disk 格式**：无 schema 变更。新增的 `.sig` 仅是 tmp dir 内的临时下载文件（与 `checksums.txt` 同生命周期，随 `os.RemoveAll(tmp)` checker.go:248 清理）。备份/staging 文件格式不变。
- **config flag / env**：
  - 新增 env，建议 `NAOZHI_UPGRADE_REQUIRE_SIGNATURE`（`1`/`0`），或复用 config。过渡期默认 `best-effort`，目标态默认 `require`。
  - 保留 `NAOZHI_UPGRADE_PIN_SHA256`（selfupdate.go:129）—— **正交防线，不移除**。pin 防"换 checksums.txt"、签名防"无发布者授权"，二者叠加；pin 仍是签名私钥泄露时的额外缓解（攻击者还得绕过 pin）。
- **install.sh 迁移**（首次安装路径，:195-212）：shell 端验签需要 `minisign` 或 `openssl` 处理 ed25519，且要把公钥写进脚本。本 RFC **给出方向**：install.sh 可分期跟进（先保持 checksum-only + 文档提示 cosign/minisign 手动验证，待客户端签名链稳定后再加 shell 验签）。客户端（selfupdate 包）是主战场，install.sh 是次级。
- **迁移路径**：见 §8。

---

## 8. Rollout plan

### Phase 1 是否可独立安全落地

**否（`hasLandablePhase1=false`）**。理由（保守判断，符合 triage）：

- 需要 **生成并托管新的 ed25519 发布密钥**（私钥进 Actions secret 或离线签名流程）——这是新签名基础设施，必须先评审密钥保管/轮换流程，不能由本轮代码先斩后奏。
- 需要 **把公钥常量编译进客户端**：一旦发版，这个 trust set 就被钉死在已部署的二进制里，错了会全网自锁（§5 最大风险）。公钥值本身需要评审确认来源可信。
- 需要 **release.yml 与客户端验签逻辑配对上线**，跨"流水线 + 客户端 + 过渡 flag 语义"三处联动，且默认行为（best-effort vs require）的选择需要产品/运维拍板。
- 这正属于 triage 列出的"需新密钥/签名基础设施"类别——**本轮不落地代码**。

唯一勉强可独立先做、low-risk 的预备件（**仅在评审批准后**）是一个 **纯函数 `verifySignature(payload, sig []byte, trustSet []ed25519.PublicKey) (keyID string, err error)`** + 其单测——它不接线进 `Download`、不嵌入真实公钥、不改流水线，纯标准库 `crypto/ed25519`。但即便这个，也建议随主方案一起评审落地，避免"半截 API 悬空"。故本轮 RFC 阶段不写。

### 文件级改动清单（供评审，**非本轮落地**）

| 文件 | 改动 | 预估行数 |
|---|---|---|
| `internal/selfupdate/signature.go`（新建） | `verifySignature` 纯函数 + 内嵌 trust set 常量 + sig 文件读取 | ~60-90 |
| `internal/selfupdate/selfupdate.go` | `Download` 内 :167-170 之间插 `fetchFile(.sig)` + `verifySignature`，**在 chmod :175 之前**；`Release` struct 加 `SigURL`；`LatestRelease` 拼 `.sig` URL（:108 附近） | ~25-40 |
| `internal/selfupdate/signature_test.go`（新建） | §4 全部 unit 测试 | ~150-220 |
| `internal/selfupdate/selfupdate_test.go` | §4 集成测试 + chmod 顺序测试，扩展 httptest 提供 .sig | ~80-120 |
| `.github/workflows/release.yml` | 新增 sign 步骤（生成 `checksums.txt.sig`）+ 上传 asset（:84/:99 附近）+ CI 自检验签 | ~15-30 |
| `cmd/naozhi/upgrade.go` | 无需改（透传 `Download`/`Release`），除非加显式 `--require-signature` flag | 0-15 |
| `install.sh` | 分期，本期可不动 | 0 |

**主方案生产代码预估：~110-180 行**（不含测试）；含测试约 350-500 行。

### 分阶段切换（flag-gated）

1. **Stage 0（流水线先行）**：release.yml 开始产出 `.sig`。客户端尚未验签。零客户端风险。
2. **Stage 1（客户端 best-effort）**：客户端默认 `best-effort`——有 .sig 且验签失败 → 拒绝（hard）；无 .sig（旧 release）→ warn 放行。emit SECURITY warn + IM notice。观察一两个发版周期。
3. **Stage 2（默认 require）**：所有线上 release 已带有效 .sig 后，把默认翻成 `require`：无 .sig 或验签失败一律 hard-fail。`NAOZHI_UPGRADE_REQUIRE_SIGNATURE=0` 作为逃生舱（应急降级）。
4. **Stage 3（install.sh + 密钥轮换演练）**：补 shell 端验签；演练一次多公钥 trust set 轮换，确认 G4 闭环。

---

## 附：8 节自检

1. Background & problem — ✅（含 file:line、issue #1738、可演示缺口）
2. Goals & non-goals — ✅（6 goals / 5 non-goals）
3. Alternatives — ✅（机制 A1/A2/A3 + 对象 B1/B2，均给胜出理由）
4. Test strategy — ✅（点名 unit/集成/regression + 防自锁 CI 自检）
5. Risk & rollback — ✅（点名 chmod 顺序、O_EXCL/原子 rename、无锁包级状态、pin 测试）
6. Observability — ✅（log/notice/SECURITY warn；metric/dashboard 给出 N/A 理由）
7. Compatibility & migration — ✅（向后兼容、env flag、保留 pin、install.sh 迁移）
8. Rollout plan — ✅（明确 `hasLandablePhase1=false` + 文件级清单 + 行数 + 分阶段）
