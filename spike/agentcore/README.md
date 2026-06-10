# AgentCore 云上沙箱 Phase 0 spike

`docs/rfc/agentcore-cloud-sandbox.md` 的 Phase 0 验证产物（**spike 代码，不进 naozhi 二进制**，
`internal/` 不得 import 本目录）。实测记录见 `docs/rfc/agentcore-cloud-sandbox-validation.md`。

| 文件 | 作用 |
|---|---|
| `bootstrap/` | microVM 容器入口（Go）：`/ping` + `/invocations`，物化 payload → spawn claude CLI → SSE 回传 + 15s keepalive |
| `Dockerfile` | base 镜像：amazonlinux:2023 (ARM64) + claude CLI + bootstrap，非 root |
| `build.sh` | 构建并推送 ECR `naozhi-sandbox:<tag>`（从宿主 `which claude` 取二进制） |

Phase 1 时 bootstrap 将改为复用 naozhi 拆包后的 spawn/协议包并迁入正式构建产物。
