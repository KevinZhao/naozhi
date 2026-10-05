// cron_format.js — the cron view's text formatters, a leaf (caps.leaves):
// titles, colloquial times, the running-elapsed label, the error-class names
// and the per-job ledger figure. Pure functions of their arguments, apart from
// cronJobLedgerCostHtml reading cron_state's cost cache.

import { esc, escAttr, formatCostUSD } from './nz_util.js';
import { cronJobCostCache } from './cron_state.js';

// firstNonEmptyLine 取文本的首个非空行并按 rune 截断到 limit。
// 与后端 cron.JobTitleOrFallback 行为对齐——显式 title 为空时前后端
// 应该渲染一致的 fallback 标题。limit 默认 60 rune 匹配卡片视觉宽度。
export function firstNonEmptyLine(text, limit) {
  if (!text) return '';
  const lines = String(text).split('\n');
  let line = '';
  for (const l of lines) {
    const t = l.trim();
    if (t) { line = t; break; }
  }
  if (!line) return '';
  const max = limit > 0 ? limit : 60;
  // Array.from 处理 UTF-16 surrogate pair（emoji、非 BMP 字符），避免
  // substring 切断代理对产生替换字符。
  const chars = Array.from(line);
  if (chars.length <= max) return line;
  return chars.slice(0, max).join('') + '…';
}

// calendarDayDelta returns the number of calendar days between two epoch-ms
// (positive if `b` is later than `a` in local time). Uses local midnight so
// "昨天" / "明天" align with wall-clock date, not 24h intervals — a run
// 25h ago from now=01:00 is actually 前天, not 昨天.
export function calendarDayDelta(a, b) {
  const da = new Date(a);
  const db = new Date(b);
  const a0 = new Date(da.getFullYear(), da.getMonth(), da.getDate()).getTime();
  const b0 = new Date(db.getFullYear(), db.getMonth(), db.getDate()).getTime();
  return Math.round((b0 - a0) / 86400000);
}

// formatWhenColloquial renders a future epoch-ms as a short human-readable
// phrase for the "when" column. Buckets:
//
//   - imminent  (<10m)        → "5 分钟后"
//   - short     (<1h)          → "32 分钟后"
//   - same day                 → "约 14 小时后"
//   - tomorrow, early (<12:00) → "明早 04:00"
//   - tomorrow, late           → "明日 20:00"
//   - >=2 days                 → "3 天后 · 02:00"
//
// Returns {label, imminent} so callers choose their own highlight class.
export function formatWhenColloquial(ms) {
  if (!ms) return { label: '—', imminent: false };
  const now = Date.now();
  const d = ms - now;
  if (d < 0) return { label: '即将', imminent: true };
  if (d < 60 * 1000) return { label: '片刻后', imminent: true };
  if (d < 10 * 60 * 1000) return { label: Math.max(1, Math.floor(d / 60000)) + ' 分钟后', imminent: true };
  if (d < 60 * 60 * 1000) return { label: Math.floor(d / 60000) + ' 分钟后', imminent: false };
  const dayDelta = calendarDayDelta(now, ms);
  const tgt = new Date(ms);
  const hhmm = String(tgt.getHours()).padStart(2, '0') + ':' + String(tgt.getMinutes()).padStart(2, '0');
  if (dayDelta === 0) {
    return { label: '约 ' + Math.floor(d / 3600000) + ' 小时后', imminent: false };
  }
  if (dayDelta === 1) {
    const prefix = tgt.getHours() < 12 ? '明早' : '明日';
    return { label: prefix + ' ' + hhmm, imminent: false };
  }
  return { label: dayDelta + ' 天后 · ' + hhmm, imminent: false };
}

// formatAgoColloquial — past epoch-ms → short Chinese "刚刚 / 3 分钟前 /
// 2 小时前 / 昨天 HH:MM / 3 天前". Uses calendar days so "昨天" means
// yesterday's date, not 24-48h ago (a 25h-old run from 01:00 is 前天).
export function formatAgoColloquial(ms) {
  if (!ms) return '';
  const now = Date.now();
  const d = now - ms;
  if (d < 60 * 1000) return '刚刚';
  if (d < 60 * 60 * 1000) return Math.floor(d / 60000) + ' 分钟前';
  const dayDelta = calendarDayDelta(ms, now);
  if (dayDelta === 0) return Math.floor(d / 3600000) + ' 小时前';
  const tgt = new Date(ms);
  const pad = n => (n < 10 ? '0' + n : '' + n);
  if (dayDelta === 1) return '昨天 ' + pad(tgt.getHours()) + ':' + pad(tgt.getMinutes());
  return dayDelta + ' 天前';
}

// formatRunningElapsed returns a colloquial "正在运行 12s / 2m" label for
// the inline badge. Floors to seconds; wraps to "Nm Ss" past 60s.
export function formatRunningElapsed(startedAt) {
  if (!startedAt) return '正在运行';
  const ms = Date.now() - startedAt;
  if (ms < 0) return '正在运行';
  const sec = Math.floor(ms / 1000);
  if (sec < 60) return '运行中 ' + sec + 's';
  const m = Math.floor(sec / 60);
  const s = sec - m * 60;
  if (m < 60) return '运行中 ' + m + 'm ' + s + 's';
  const h = Math.floor(m / 60);
  return '运行中 ' + h + 'h ' + (m - h * 60) + 'm';
}

// cronErrorClassLabel —— 后端 ErrorClass 枚举的中文友好名。RFC §9 错误分类映射。
// 未知值原样返回，方便排查（不应发生但容错）。
const CRON_ERROR_CLASS_LABELS = Object.freeze({
  session_error: '会话错误',
  send_error: '发送失败',
  turn_failed: '后端报错',
  deadline_exceeded: '超时',
  canceled: '已取消',
  workdir_unreachable: '工作目录不可达',
  workdir_outside_root: '工作目录越界',
  overlap_skipped: '重叠跳过',
  session_capacity: '会话上限跳过',
  router_missing: '路由未就绪',
  paused_concurrent: '暂停时被抢',
  deleted_concurrent: '运行中被删除',
  panic: '内部异常',
  // interrupted 与 canceled 同为 RunState=canceled，区别是谁中止的：进程自己没了
  // （drain 超预算或被硬杀）。措辞须与"已取消"分开，免得把被杀的运行读成自己点过取消。
  interrupted: '进程中断（未跑完）',
  // 重启存活的 CLI 被启动时的 argv 漂移检查关掉：是操作员自己的配置修改
  // 结束了这次 run，不是重启本身 —— 与 interrupted 分开命名，操作员才
  // 知道该看的是自己改了什么，而不是找一个不存在的崩溃（#2749 语义）。
  config_drift: '配置变更中止（升级时改了模型/参数）',
  // 云沙箱三态（agentcore-cloud-sandbox RFC §6.1/§7.2）。transport 是
  // §6.2 双跑风险态：流断了但 microVM 状态未知，徽标走红色 + ⚠。
  sandbox_failed: '云沙箱任务失败',
  sandbox_transport: '云沙箱断流（状态未知）',
  sandbox_unavailable: '云沙箱未配置',
});
export function cronErrorClassLabel(cls) {
  return Object.hasOwn(CRON_ERROR_CLASS_LABELS, cls) ? CRON_ERROR_CLASS_LABELS[cls] : (cls || '');
}

// cronJobLedgerCostHtml renders the job's 30-day ledger figure (runs + session
// spend outside a run window) or '' before the fetch lands / nothing was spent.
export function cronJobLedgerCostHtml(jobId) {
  const c = cronJobCostCache[jobId];
  if (!c || !(c.usd > 0)) return '';
  const title = '近 30 天账本合计：' + c.entries + ' 条账本记录（本地 / 云沙箱运行，及运行窗口外由会话记的账），CLI 估算口径' +
    (c.dropped > 0 ? '；账本曾丢弃 ' + c.dropped + ' 条，可能偏低' : '');
  return '<span class="ct-cost-ledger" title="' + escAttr(title) + '">30 天 ' + esc(formatCostUSD(c.usd)) + '</span>';
}
