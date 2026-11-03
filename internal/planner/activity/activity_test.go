package activity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestFromCommand(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cmd  string
		want []Type // nil = not a state change
	}{
		// compound: every state change counts
		{"git push -u origin feat/x && gh pr create --title t --body b", []Type{TypeGitPush, TypePRCreate}},
		{"gh pr merge 12 --squash && git tag v1 && git push --tags", []Type{TypePRMerge, TypeGitTag, TypeGitPush}},
		// git
		{"git push", []Type{TypeGitPush}},
		{"git push -u origin feat/x", []Type{TypeGitPush}},
		{"timeout 90 git -c http.version=HTTP/1.1 push --no-verify -u origin b", []Type{TypeGitPush}},
		{"cd /repo && git -C sub push origin HEAD", []Type{TypeGitPush}},
		{"GIT_TRACE=1 git push --tags", []Type{TypeGitPush}},
		{"git push --dry-run origin main", nil},
		{"git push -n", nil},
		{"git status && git log --oneline -3", nil},
		{"git ls-remote origin refs/heads/x", nil},
		{"echo 'git push' # not run", nil},
		{"git tag v1.2.3", []Type{TypeGitTag}},
		{"git tag -a v1.2.3 -m 'release'", []Type{TypeGitTag}},
		{"git tag", nil},
		{"git tag -l 'v*'", nil},
		{"git tag --contains abc123", nil},
		{"git tag --sort=-v:refname | head", nil},
		// gh
		{`gh pr create --base master --title "feat: x" --body "$(cat <<'EOF'\nbody\nEOF\n)"`, []Type{TypePRCreate}},
		{"gh pr merge 123 --squash", []Type{TypePRMerge}},
		{"gh pr merge 12 --auto --squash", nil},
		{"gh pr merge 12 --disable-auto", nil},
		{"gh pr view 123", nil},
		{"gh pr list --state open", nil},
		{"gh pr checks 123", nil},
		{"gh release create v1.0.0 --notes x", []Type{TypeRelease}},
		{"gh release view v1.0.0", nil},
		// release scripts
		{"./scripts/release.sh v1", []Type{TypeRelease}},
		{"bash scripts/publish.sh", []Type{TypeRelease}},
		{"make release", []Type{TypeRelease}},
		{"make -C deploy VERSION=1 release-linux", []Type{TypeRelease}},
		{"make build", nil},
		{"npm publish", []Type{TypeRelease}},
		{"npm publish --dry-run", nil},
		{"goreleaser release --clean", []Type{TypeRelease}},
		{"goreleaser release --snapshot", nil},
		{"release-notes.txt", nil},
		// aws: writes
		{"aws ec2 run-instances --image-id ami-1", []Type{TypeAWSWrite}},
		{"aws --region us-east-1 s3api put-object --bucket b --key k", []Type{TypeAWSWrite}},
		{"AWS_PROFILE=x aws lambda update-function-code --function-name f", []Type{TypeAWSWrite}},
		{"aws dynamodb batch-write-item --request-items file://x.json", []Type{TypeAWSWrite}},
		{"aws s3 cp ./out.html s3://bucket/out.html", []Type{TypeAWSWrite}},
		{"aws s3 sync dist s3://bucket/site", []Type{TypeAWSWrite}},
		{"aws s3 rm s3://bucket/k", []Type{TypeAWSWrite}},
		{"aws cloudformation deploy --template-file t.yaml", []Type{TypeAWSWrite}},
		{"aws cognito-idp admin-create-user --user-pool-id p", []Type{TypeAWSWrite}},
		{"aws s3 cp out.html s3://bucket/x.html --region us-east-1", []Type{TypeAWSWrite}},
		{"aws s3 sync dist s3://bucket --profile prod --exclude '*.tmp'", []Type{TypeAWSWrite}},
		{"aws s3 cp ./a s3://b/ --acl public-read --recursive", []Type{TypeAWSWrite}},
		{"aws ec2 --region us-east-1 run-instances --image-id x", []Type{TypeAWSWrite}},
		// aws: reads
		{"aws sts get-caller-identity", nil},
		{"aws --profile p --output json ec2 describe-instances", nil},
		{"aws s3 ls s3://bucket", nil},
		{"aws s3 cp s3://bucket/k ./local", nil},
		{"aws s3 cp s3://bucket/k ./local --region us-east-1 --profile s3://x", nil},
		{"aws s3 sync s3://bucket ./site --exclude '*.tmp'", nil},
		{"aws dynamodb batch-get-item --request-items x", nil},
		{"aws dynamodb scan --table-name t", nil},
		{"aws logs tail /aws/lambda/f --follow", nil},
		{"aws sts assume-role --role-arn r", nil},
		{"aws ec2 run-instances --dry-run --image-id ami-1", nil},
		{"aws configure set region us-east-1", nil},
		{"aws ec2 help", nil},
		{"ls -la", nil},
	}
	for _, c := range cases {
		acts := FromCommand(c.cmd)
		var got []Type
		for _, a := range acts {
			got = append(got, a.Type)
			if a.Summary == "" {
				t.Errorf("FromCommand(%q): %s has no summary", c.cmd, a.Type)
			}
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("FromCommand(%q) = %+v, want types %v", c.cmd, acts, c.want)
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
		if acts := FromCommand(cmd); len(acts) != 1 || acts[0].Summary != want {
			t.Errorf("FromCommand(%q) = %+v, want one with summary %q", cmd, acts, want)
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
		{"asana read with write word", "mcp__enterprise-asana-mcp__asana___GetStatusUpdate", nil, "", ""},
		{"asana noun first", "mcp__asana__asana_create_task", nil, TypeAsana, ""},
		{"asana via gateway", "mcp__gw__asana___AddFollowersToTask", nil, TypeAsana, ""},
		{"mail send", "mcp__aws-outlook-mcp__email_send", in(map[string]string{"subject": "Hi"}), TypeEmail, ""},
		{"mail reply", "mcp__aws-outlook-mcp__email_reply", nil, TypeEmail, ""},
		{"mail search", "mcp__aws-outlook-mcp__email_search", nil, "", ""},
		{"mail enable writes", "mcp__aws-outlook-mcp__enable_write_tools", nil, "", ""},
		{"mail list with write word", "mcp__gmail__list_send_as", nil, "", ""},
		{"slack post", "mcp__slack-mcp__post_message", nil, "", ""},
	}
	for _, c := range cases {
		acts, path := Classify(c.tool, c.input)
		if path != c.wantPath {
			t.Errorf("%s: path = %q, want %q", c.name, path, c.wantPath)
		}
		if c.want == "" {
			if len(acts) != 0 {
				t.Errorf("%s: got %+v, want no activity", c.name, acts)
			}
			continue
		}
		if len(acts) != 1 || acts[0].Type != c.want {
			t.Errorf("%s: got %+v; want type %s", c.name, acts, c.want)
		}
	}
	if acts, _ := Classify("mcp__aws-outlook-mcp__email_send", in(map[string]string{"subject": "Hi"})); len(acts) != 1 || acts[0].Summary != "email_send: Hi" {
		t.Errorf("mail = %+v, want summary %q", acts, "email_send: Hi")
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
	type relCase struct {
		root, abs, want string
		ok              bool
	}
	cases := []relCase{
		{"/r", "/r/docs/a.md", "docs/a.md", true},
		{"/r", "/r/./x/../a.md", "a.md", true},
		{"/r", "/other/a.md", "", false},
		{"/r", "/r", "", false},
		{"/r", "docs/a.md", "", false},
		{"", "/r/a.md", "", false},
	}
	// A root stored resolved and a path spelled through a symlink to it (the
	// workspace is validated resolved; macOS /tmp is a symlink).
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases,
		relCase{resolved, filepath.Join(link, "new", "a.md"), "new/a.md", true},
		relCase{link, filepath.Join(resolved, "a.md"), "a.md", true},
	)
	for _, c := range cases {
		got, ok := RelPath(c.root, c.abs)
		if got != c.want || ok != c.ok {
			t.Errorf("RelPath(%q, %q) = %q, %v; want %q, %v", c.root, c.abs, got, ok, c.want, c.ok)
		}
	}
}
