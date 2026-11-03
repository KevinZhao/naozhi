package activity

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestFromCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cmd  string
		want Type // "" = not a state change
	}{
		// git
		{"git push", TypeGitPush},
		{"git push -u origin feat/x", TypeGitPush},
		{"timeout 90 git -c http.version=HTTP/1.1 push --no-verify -u origin b", TypeGitPush},
		{"cd /repo && git -C sub push origin HEAD", TypeGitPush},
		{"GIT_TRACE=1 git push --tags", TypeGitPush},
		{"git push --dry-run origin main", ""},
		{"git push -n", ""},
		{"git status && git log --oneline -3", ""},
		{"git ls-remote origin refs/heads/x", ""},
		{"echo 'git push' # not run", ""},
		{"git tag v1.2.3", TypeGitTag},
		{"git tag -a v1.2.3 -m 'release'", TypeGitTag},
		{"git tag", ""},
		{"git tag -l 'v*'", ""},
		{"git tag --contains abc123", ""},
		{"git tag --sort=-v:refname | head", ""},
		// gh
		{`gh pr create --base master --title "feat: x" --body "$(cat <<'EOF'\nbody\nEOF\n)"`, TypePRCreate},
		{"gh pr merge 123 --squash", TypePRMerge},
		{"gh pr view 123", ""},
		{"gh pr list --state open", ""},
		{"gh pr checks 123", ""},
		{"gh release create v1.0.0 --notes x", TypeRelease},
		{"gh release view v1.0.0", ""},
		// release scripts
		{"./scripts/release.sh v1", TypeRelease},
		{"bash scripts/publish.sh", TypeRelease},
		{"make release", TypeRelease},
		{"make -C deploy VERSION=1 release-linux", TypeRelease},
		{"make build", ""},
		{"npm publish", TypeRelease},
		{"npm publish --dry-run", ""},
		{"goreleaser release --clean", TypeRelease},
		{"goreleaser release --snapshot", ""},
		{"release-notes.txt", ""},
		// aws: writes
		{"aws ec2 run-instances --image-id ami-1", TypeAWSWrite},
		{"aws --region us-east-1 s3api put-object --bucket b --key k", TypeAWSWrite},
		{"AWS_PROFILE=x aws lambda update-function-code --function-name f", TypeAWSWrite},
		{"aws dynamodb batch-write-item --request-items file://x.json", TypeAWSWrite},
		{"aws s3 cp ./out.html s3://bucket/out.html", TypeAWSWrite},
		{"aws s3 sync dist s3://bucket/site", TypeAWSWrite},
		{"aws s3 rm s3://bucket/k", TypeAWSWrite},
		{"aws cloudformation deploy --template-file t.yaml", TypeAWSWrite},
		{"aws cognito-idp admin-create-user --user-pool-id p", TypeAWSWrite},
		// aws: reads
		{"aws sts get-caller-identity", ""},
		{"aws --profile p --output json ec2 describe-instances", ""},
		{"aws s3 ls s3://bucket", ""},
		{"aws s3 cp s3://bucket/k ./local", ""},
		{"aws dynamodb batch-get-item --request-items x", ""},
		{"aws dynamodb scan --table-name t", ""},
		{"aws logs tail /aws/lambda/f --follow", ""},
		{"aws sts assume-role --role-arn r", ""},
		{"aws ec2 run-instances --dry-run --image-id ami-1", ""},
		{"aws configure set region us-east-1", ""},
		{"aws ec2 help", ""},
		{"ls -la", ""},
	}
	for _, c := range cases {
		a, ok := FromCommand(c.cmd)
		if c.want == "" {
			if ok {
				t.Errorf("FromCommand(%q) = %+v, want no activity", c.cmd, a)
			}
			continue
		}
		if !ok || a.Type != c.want || a.Summary == "" {
			t.Errorf("FromCommand(%q) = %+v, %v; want type %s", c.cmd, a, ok, c.want)
		}
	}
}

func TestFromCommand_Summary(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"cd /r && timeout 90 git push --no-verify -u origin b": "git push --no-verify -u origin b",
		"aws --region x ec2 run-instances --image-id ami-1":    "aws ec2 run-instances",
		`gh pr create --title "a b" --body "long"`:             "gh pr create",
	}
	for cmd, want := range cases {
		if a, _ := FromCommand(cmd); a.Summary != want {
			t.Errorf("FromCommand(%q).Summary = %q, want %q", cmd, a.Summary, want)
		}
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	in := func(v map[string]string) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	cases := []struct {
		name     string
		tool     string
		input    json.RawMessage
		want     Type
		wantPath string
	}{
		{"write md", "Write", in(map[string]string{"file_path": "/r/docs/a.md", "content": "x"}), TypeOutput, "/r/docs/a.md"},
		{"edit html upper ext", "Edit", in(map[string]string{"file_path": "/r/A.HTML"}), TypeOutput, "/r/A.HTML"},
		{"write go is not output", "Write", in(map[string]string{"file_path": "/r/main.go"}), "", "/r/main.go"},
		{"bash", "Bash", in(map[string]string{"command": "git push"}), TypeGitPush, ""},
		{"read", "Read", in(map[string]string{"file_path": "/r/a.md"}), "", ""},
		{"asana create", "mcp__enterprise-asana-mcp__asana___CreateTask", in(map[string]string{"name": "Ship it"}), TypeAsana, ""},
		{"asana update story", "mcp__enterprise-asana-mcp__asana___UpdateStory", nil, TypeAsana, ""},
		{"asana read", "mcp__enterprise-asana-mcp__asana___GetTaskDetails", nil, "", ""},
		{"asana search", "mcp__enterprise-asana-mcp__asana___AsanaSearch", nil, "", ""},
		{"mail send", "mcp__aws-outlook-mcp__email_send", in(map[string]string{"subject": "Hi"}), TypeEmail, ""},
		{"mail reply", "mcp__aws-outlook-mcp__email_reply", nil, TypeEmail, ""},
		{"mail search", "mcp__aws-outlook-mcp__email_search", nil, "", ""},
		{"mail enable writes", "mcp__aws-outlook-mcp__enable_write_tools", nil, "", ""},
		{"slack post", "mcp__slack-mcp__post_message", nil, "", ""},
	}
	for _, c := range cases {
		a, path, ok := Classify(c.tool, c.input)
		if path != c.wantPath {
			t.Errorf("%s: path = %q, want %q", c.name, path, c.wantPath)
		}
		if c.want == "" {
			if ok {
				t.Errorf("%s: got %+v, want no activity", c.name, a)
			}
			continue
		}
		if !ok || a.Type != c.want {
			t.Errorf("%s: got %+v, %v; want type %s", c.name, a, ok, c.want)
		}
	}
	if a, _, _ := Classify("mcp__aws-outlook-mcp__email_send", in(map[string]string{"subject": "Hi"})); a.Summary != "email_send: Hi" {
		t.Errorf("mail summary = %q", a.Summary)
	}
}

func TestPRLinks(t *testing.T) {
	t.Parallel()
	text := "https://github.com/o/r/pull/12\nremote: https://github.com/o/r/pull/new/feat\nsee https://github.com/o/r/pull/12 and https://github.com/o-2/r.js/pull/7."
	want := []string{"https://github.com/o/r/pull/12", "https://github.com/o-2/r.js/pull/7"}
	if got := PRLinks(text); !slices.Equal(got, want) {
		t.Errorf("PRLinks = %v, want %v", got, want)
	}
	if got := PRLinks("https://github.com/o/r/issues/3"); got != nil {
		t.Errorf("issue link = %v, want none", got)
	}
}

func TestRelPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		root, abs, want string
		ok              bool
	}{
		{"/r", "/r/docs/a.md", "docs/a.md", true},
		{"/r", "/r/./x/../a.md", "a.md", true},
		{"/r", "/other/a.md", "", false},
		{"/r", "/r", "", false},
		{"/r", "docs/a.md", "", false},
		{"", "/r/a.md", "", false},
	}
	for _, c := range cases {
		got, ok := RelPath(c.root, c.abs)
		if got != c.want || ok != c.ok {
			t.Errorf("RelPath(%q, %q) = %q, %v; want %q, %v", c.root, c.abs, got, ok, c.want, c.ok)
		}
	}
}
