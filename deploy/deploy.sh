#!/bin/bash
# Naozhi deployment script
# Usage: ./deploy/deploy.sh [build|setup|deploy|status|logs]
set -e

REGION="ap-northeast-1"
INSTANCE_ID=""  # Set to target EC2 instance ID
S3_BUCKET="naozhi-deploy-tmp"

# ============================================================
# Helpers
# ============================================================

ssm_run() {
  local instance="$1"
  shift
  local cmd_id
  cmd_id=$(aws ssm send-command \
    --instance-ids "$instance" \
    --document-name "AWS-RunShellScript" \
    --timeout-seconds 60 \
    --parameters commands="$1" \
    --region "$REGION" \
    --query 'Command.CommandId' --output text)
  sleep "${2:-10}"
  aws ssm get-command-invocation \
    --command-id "$cmd_id" \
    --instance-id "$instance" \
    --region "$REGION" \
    --query '[Status,StandardOutputContent]' --output text
}

# ============================================================
# Commands
# ============================================================

cmd_build() {
  echo "=== Building naozhi ==="
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/naozhi ./cmd/naozhi/
  echo "Built: bin/naozhi ($(du -h bin/naozhi | cut -f1))"
}

cmd_upload() {
  echo "=== Uploading to S3 ==="
  aws s3 cp bin/naozhi "s3://$S3_BUCKET/naozhi" --region "$REGION"
  aws s3 cp config.yaml "s3://$S3_BUCKET/config.yaml" --region "$REGION"
  aws s3 cp deploy/naozhi.service "s3://$S3_BUCKET/naozhi.service" --region "$REGION"
  echo "Uploaded to s3://$S3_BUCKET/"
}

cmd_deploy() {
  if [ -z "$INSTANCE_ID" ]; then
    echo "Error: Set INSTANCE_ID in this script first"
    exit 1
  fi

  cmd_build
  cmd_upload

  echo "=== Deploying to $INSTANCE_ID ==="
  ssm_run "$INSTANCE_ID" '["sudo -u ec2-user bash -c \"mkdir -p ~/naozhi/bin ~/.naozhi && aws s3 cp s3://'"$S3_BUCKET"'/naozhi ~/naozhi/bin/naozhi && chmod +x ~/naozhi/bin/naozhi && aws s3 cp s3://'"$S3_BUCKET"'/config.yaml ~/naozhi/config.yaml\"", "aws s3 cp s3://'"$S3_BUCKET"'/naozhi.service /etc/systemd/system/naozhi.service && systemctl daemon-reload && systemctl restart naozhi && sleep 2 && systemctl status naozhi --no-pager | head -5"]' 15

  echo ""
  echo "=== Health check ==="
  sleep 3
  curl -s "https://${DOMAIN:-localhost:8180}/health" | python3 -m json.tool 2>/dev/null || echo "health check pending..."
}

cmd_status() {
  if [ -z "$INSTANCE_ID" ]; then
    echo "Error: Set INSTANCE_ID"
    exit 1
  fi
  ssm_run "$INSTANCE_ID" '["systemctl status naozhi --no-pager"]' 5
}

cmd_logs() {
  if [ -z "$INSTANCE_ID" ]; then
    echo "Error: Set INSTANCE_ID"
    exit 1
  fi
  ssm_run "$INSTANCE_ID" '["journalctl -u naozhi --since \"5 min ago\" --no-pager"]' 5
}

# ============================================================
# Main
# ============================================================

case "${1:-deploy}" in
  build)  cmd_build ;;
  upload) cmd_upload ;;
  deploy) cmd_deploy ;;
  status) cmd_status ;;
  logs)   cmd_logs ;;
  *)
    echo "Usage: $0 [build|upload|deploy|status|logs]"
    exit 1
    ;;
esac
