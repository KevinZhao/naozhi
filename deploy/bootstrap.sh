#!/bin/bash
# Bootstrap a new EC2 instance for Naozhi
# Run via SSM or user-data on a fresh Amazon Linux 2023 ARM64 instance
set -e

echo "=== Installing Node.js ==="
curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash
export NVM_DIR="$HOME/.nvm"
[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"
nvm install 22
nvm use 22

echo "=== Installing Claude CLI ==="
# Claude CLI binary should be uploaded to S3 and pulled:
#   aws s3 cp s3://naozhi-deploy-tmp/claude-cli ~/.local/share/claude/versions/$(claude_version)
#   ln -sf ~/.local/share/claude/versions/$(claude_version) ~/.local/bin/claude
# Or install via official script (requires internet access to claude.ai):
#   curl -fsSL https://claude.ai/install.sh | sh

echo "=== Creating directories ==="
mkdir -p ~/naozhi/bin ~/.naozhi

echo "=== Bootstrap complete ==="
echo "Next steps:"
echo "  1. Deploy naozhi binary: aws s3 cp s3://naozhi-deploy-tmp/naozhi ~/naozhi/bin/naozhi"
echo "  2. Setup Feishu env: ./deploy/setup-env.sh <instance-id>"
echo "  3. Install systemd service and start"
