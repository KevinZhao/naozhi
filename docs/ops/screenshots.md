# Dashboard 截图工具

`scripts/dashboard-screenshots.js` 用 Playwright + 无头 Chromium 拉取 naozhi Dashboard 的一组关键状态截图，供 UI 改动 review / 前后对比使用。

源自 Round 110 的 ad-hoc `/tmp/naozhi-shots/capture*.js`，这里做了参数化 + 错误隔离 + 稳定选择器替换。

## 先决条件

Playwright 已通过 `test/e2e/package.json` 拉入，直接复用：

```bash
(cd test/e2e && npm install)
```

如果宿主 npm 拒绝符号链接到仓外目录，可在仓根单独装：

```bash
npm install --no-save --prefix . playwright
# 或把 playwright 加入 test/e2e/package.json 后重新 npm install
```

截图会写到 `tmp/naozhi-shots/`（相对于仓根）。该目录已在 `.gitignore` 内（若没有请追加）。

## 用法

```bash
NAOZHI_DASHBOARD_TOKEN=$(sudo grep NAOZHI_DASHBOARD_TOKEN /home/ec2-user/.naozhi/env | cut -d= -f2-) \
  node scripts/dashboard-screenshots.js
```

自定义：

| 环境变量 | 默认值 | 作用 |
|---|---|---|
| `NAOZHI_DASHBOARD_TOKEN` | 必填 | 登录 token，同 `config.yaml` |
| `NAOZHI_BASE_URL` | `http://localhost:8180` | 目标实例 |
| `NAOZHI_SHOTS_DIR` | `tmp/naozhi-shots` | 输出目录（相对仓根） |

## 产物

| 文件 | 内容 |
|---|---|
| `01-login.png` | 未登录的 `/dashboard` 登录视图 |
| `02-dashboard-desktop.png` | 登录后桌面空闲态（fullPage） |
| `02b-dashboard-viewport.png` | 同上但仅视口（首屏） |
| `03-dashboard-mobile.png` | 390×844 移动视口 |
| `06-new-session-modal.png` | 新建会话 command palette |
| `07-history-drawer.png` | 历史会话抽屉 |
| `08-cron-panel.png` | Cron 面板 |
| `09-help.png` | `?` 快捷键 modal |
| `dom-summary.txt` | 最后一次访问页面的 DOM 大纲（深度 3） |
| `console.log` | 整次捕获期间的浏览器 console + pageerror |

## 退出码

| 码 | 含义 |
|---|---|
| 0 | 至少一张截图成功 |
| 1 | `NAOZHI_DASHBOARD_TOKEN` 未设置 |
| 2 | 登录 API 失败（check 服务存活 / token 正确） |
| 3 | Playwright 未安装或每一步都失败 |

单步失败会 `[warn]` 记录，整体继续跑下一步 —— 单个 flaky 选择器不拉垮其他截图。

## 配对 review

做 UI 改动前先跑一遍，把产物 tar 到 `tmp/naozhi-shots/before/`；改动后再跑到 `after/`。然后：

```bash
diff -q tmp/naozhi-shots/before tmp/naozhi-shots/after
# 或
xdg-open tmp/naozhi-shots/02-dashboard-desktop.png
```

文件命名固定让两次产物可以按文件名一对一对照。DOM 大纲 diff 往往比像素 diff 更能看出结构性变化。
