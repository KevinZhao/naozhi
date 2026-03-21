#!/bin/bash
# Setup Feishu credentials on the target instance
# Usage: ./deploy/setup-env.sh <instance-id>
set -e

INSTANCE_ID="${1:?Usage: $0 <instance-id>}"
REGION="ap-northeast-1"

read -p "Feishu App ID: " APP_ID
read -p "Feishu App Secret: " APP_SECRET
read -p "Feishu Verification Token: " VERIFY_TOKEN
read -p "Feishu Encrypt Key (empty if none): " ENCRYPT_KEY

CMD_ID=$(aws ssm send-command \
  --instance-ids "$INSTANCE_ID" \
  --document-name "AWS-RunShellScript" \
  --timeout-seconds 30 \
  --parameters commands="[\"sudo -u ec2-user bash -c \\\"mkdir -p ~/.naozhi && cat > ~/.naozhi/env << 'EOF'\nIM_APP_ID=$APP_ID\nIM_APP_SECRET=$APP_SECRET\nIM_VERIFICATION_TOKEN=$VERIFY_TOKEN\nIM_ENCRYPT_KEY=$ENCRYPT_KEY\nEOF\nchmod 600 ~/.naozhi/env && echo DONE\\\"\"]" \
  --region "$REGION" \
  --query 'Command.CommandId' --output text)

sleep 5
aws ssm get-command-invocation \
  --command-id "$CMD_ID" \
  --instance-id "$INSTANCE_ID" \
  --region "$REGION" \
  --query '[Status,StandardOutputContent]' --output text

echo ""
echo "Restart naozhi to apply: systemctl restart naozhi"
