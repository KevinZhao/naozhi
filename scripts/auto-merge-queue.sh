#!/usr/bin/env bash
# auto-merge-queue.sh — 串行 PR 合并队列推进器（naozhi cron 调用）
#
# 为什么不用 GitHub Actions:
# 个人 user 仓库无 GitHub Merge Queue (Team plan only)。Actions workflow
# 用 GITHUB_TOKEN 调 gh pr update-branch 产生 github-actions[bot] 身份的
# merge commit，GitHub 防循环规则导致 CI 不会被触发，PR 永远 BLOCKED。
#
# 本脚本由 naozhi cron 在服务器上执行，使用 ~/.config/gh/hosts.yml 里的
# OAuth token (gho_...) 调 gh，产生的 merge commit author 是真实用户，
# CI 正常触发。
#
# 串行设计:
# 每次只处理"队首"PR (按 createdAt 正序)。每合一个就让其余 PR 变 BEHIND
# 重跑 CI——并发更新没意义反而浪费 Actions 配额。状态机驱动，cron 多次
# 唤醒推进。
#
# 状态机:
#   DIRTY    → 留 comment，跳到下一个 PR
#   BEHIND   → gh pr update-branch (PAT)，触发 CI，退出本轮
#   BLOCKED  → 看 statusCheckRollup
#                FAILURE 全是已知 flaky → 推 empty commit 重跑
#                FAILURE 含真实失败    → 留 comment 跳过
#                IN_PROGRESS / 0 ck    → 啥也不做
#   CLEAN    → auto-merge 自己处理，啥也不做
#   QUEUE 空 → 退出

set -euo pipefail

REPO="${REPO:-KevinZhao/naozhi}"
DRY_RUN="${DRY_RUN:-0}"

# 已知 flaky 测试白名单 (扩展 grep -E 模式)
FLAKY_PATTERN='TestWS_SendAccepted|TestLinker_Resolve_RetryThenSucceed|TestDoctor_AuthStatusBranches|TestReadJSONWithRetry_eventuallyValid'

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

run() {
  if [ "$DRY_RUN" = "1" ]; then
    log "DRY-RUN: $*"
  else
    log "EXEC:    $*"
    "$@"
  fi
}

# 拿队列：所有挂了 auto-merge 且非 draft 的 open PR，按 createdAt 正序
queue_json=$(gh pr list --repo "$REPO" --state open --limit 50 \
  --json number,title,mergeStateStatus,autoMergeRequest,createdAt,isDraft,headRefName,headRefOid \
  --jq '[.[] | select(.autoMergeRequest != null and .isDraft == false)] | sort_by(.createdAt)')

queue_size=$(echo "$queue_json" | jq 'length')
if [ "$queue_size" -eq 0 ]; then
  log "queue empty"
  exit 0
fi

log "queue size: $queue_size"

process_pr() {
  local idx=$1
  local pr_json
  pr_json=$(echo "$queue_json" | jq ".[$idx]")
  local num title state branch sha
  num=$(echo "$pr_json" | jq -r .number)
  title=$(echo "$pr_json" | jq -r .title)
  state=$(echo "$pr_json" | jq -r .mergeStateStatus)
  branch=$(echo "$pr_json" | jq -r .headRefName)
  sha=$(echo "$pr_json" | jq -r .headRefOid)

  log "PR #$num [$state] $title"

  case "$state" in
    DIRTY)
      # 检查是否已留过冲突 comment（防刷屏）
      if ! gh pr view "$num" --repo "$REPO" --json comments \
           --jq '.comments[].body' | grep -q "merge 冲突"; then
        run gh pr comment "$num" --repo "$REPO" --body \
          "⚠️ 与 master 产生 merge 冲突。请在分支上 \`git merge origin/master\` 或 \`git rebase origin/master\` 解决冲突后重 push。auto-merge 标志保留，冲突解决后自动接管。"
      else
        log "  (already commented on conflict, skipping)"
      fi
      return 1  # 让队列前进到下一个
      ;;

    BEHIND)
      run gh pr update-branch "$num" --repo "$REPO"
      log "  update-branch issued, CI will retry"
      return 0  # 阻塞队列：等本 PR CI 跑完再处理后续
      ;;

    BLOCKED)
      # CI 状态：拿所有 check 的 conclusion 和 name
      local checks_json
      checks_json=$(gh pr view "$num" --repo "$REPO" --json statusCheckRollup --jq .statusCheckRollup)
      local in_progress failed_names total
      in_progress=$(echo "$checks_json" | jq '[.[] | select(.status=="IN_PROGRESS" or .status=="QUEUED")] | length')
      total=$(echo "$checks_json" | jq 'length')
      failed_names=$(echo "$checks_json" | jq -r '.[] | select(.conclusion=="FAILURE") | .name' | tr '\n' ' ')

      if [ "$total" -eq 0 ]; then
        log "  no checks yet, waiting"
        return 0
      fi

      if [ "$in_progress" -gt 0 ]; then
        log "  $in_progress check(s) still running, waiting"
        return 0
      fi

      if [ -z "$failed_names" ]; then
        # 全绿但 BLOCKED：可能是 base 已动 (BEHIND 的另一种表现) 或 review 卡
        log "  all green but BLOCKED — issuing update-branch"
        run gh pr update-branch "$num" --repo "$REPO"
        return 0
      fi

      # 有失败 check，分析是不是 flaky
      log "  failed checks: $failed_names"
      local needs_human=0
      for check_name in $failed_names; do
        # 拉失败 job 的 log，grep flaky 模式
        local detail_url
        detail_url=$(echo "$checks_json" | jq -r --arg n "$check_name" '.[] | select(.name==$n) | .detailsUrl' | head -1)
        local run_id
        run_id=$(echo "$detail_url" | grep -oE '/runs/[0-9]+' | head -1 | sed 's|/runs/||')
        if [ -z "$run_id" ]; then
          log "    cannot extract run id from $detail_url, treating as real failure"
          needs_human=1
          break
        fi
        # Match both forms:
        #   --- FAIL: TestXxx (timing)            ← classic test failure
        #   panic: Fail in goroutine after TestXxx ← goroutine-leak after t.Fatal
        local logs
        logs=$(gh run view "$run_id" --repo "$REPO" --log-failed 2>/dev/null || echo "")
        if echo "$logs" | grep -E "FAIL: ($FLAKY_PATTERN)|panic: Fail in goroutine after ($FLAKY_PATTERN)" \
           >/dev/null; then
          log "    $check_name: matches flaky pattern"
        else
          log "    $check_name: NOT flaky — real failure"
          needs_human=1
          break
        fi
      done

      if [ "$needs_human" -eq 1 ]; then
        if ! gh pr view "$num" --repo "$REPO" --json comments \
             --jq '.comments[].body' | grep -q "auto-merge-queue: real CI failure"; then
          run gh pr comment "$num" --repo "$REPO" --body \
            "🛑 auto-merge-queue: real CI failure detected (非 flaky 白名单)。失败 check: \`$failed_names\`。请人工查看：$detail_url"
        fi
        return 1  # 跳过本 PR，处理下一个
      fi

      # 全是 flaky，推 empty commit 重跑
      log "  all failures are flaky — pushing empty commit to retry"
      retry_with_empty_commit "$num" "$branch"
      return 0
      ;;

    CLEAN|UNSTABLE|HAS_HOOKS)
      log "  $state — auto-merge will handle, nothing to do"
      return 0
      ;;

    UNKNOWN)
      log "  state UNKNOWN, GitHub computing — wait"
      return 0
      ;;

    *)
      log "  unhandled state: $state"
      return 0
      ;;
  esac
}

retry_with_empty_commit() {
  local num=$1 branch=$2
  if [ "$DRY_RUN" = "1" ]; then
    log "  DRY-RUN: would push empty commit to $branch"
    return
  fi
  local tmpdir
  tmpdir=$(mktemp -d)
  trap "rm -rf '$tmpdir'" RETURN
  (
    cd "$tmpdir"
    git clone --depth 1 --branch "$branch" "https://github.com/$REPO.git" pr 2>&1 | tail -2
    cd pr
    git commit --allow-empty -m "chore: 重跑 CI（auto-merge-queue: flaky 测试重试）"
    git push origin "HEAD:$branch" 2>&1 | tail -2
  )
}

# 主循环：从队首处理，遇到"阻塞"就停止本轮，遇到"跳过"就前进
i=0
while [ "$i" -lt "$queue_size" ]; do
  if process_pr "$i"; then
    log "blocking on PR at position $i — exiting this round"
    exit 0
  fi
  i=$((i + 1))
done

log "queue traversed, no actionable PRs"
