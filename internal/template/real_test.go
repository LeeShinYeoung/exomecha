package template

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Real sbx behavior, such as loading from /dev/stdin, cannot be faked. Takes a few minutes.
func TestRealBuild(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 to build templates with the real sbx")
	}
	const from = "FROM docker/sandbox-templates:claude-code-docker\n"
	const sandbox = "impl-env-from-template"
	files := fakeFiles{
		dockerfilePath: from + "RUN true\n",
		hostsPath:      "example.com # proves the helper takes allowed-hosts\n",
	}
	e := New(files, "impl-env", "realtest")
	t.Cleanup(func() { cleanUp(t, e, sandbox) })
	ctx := t.Context()

	first, ok, err := e.Template(ctx)
	if err != nil || !ok || first != e.ref(files[dockerfilePath]) {
		t.Fatalf("Template = %q, %v, %v", first, ok, err)
	}
	wantTags(t, e, first)
	if exists, err := e.sandboxExists(ctx, e.helper()); err != nil || exists {
		t.Errorf("helper left after a build: %v, %v", exists, err)
	}
	if again, _, err := e.Template(ctx); err != nil || again != first {
		t.Errorf("Template again = %q, %v", again, err)
	}

	sbx(t, "create", "--name", sandbox, "--pull", "never", "-t", first, "claude")
	if out := sbx(t, "exec", sandbox, "sh", "-c", "echo ok"); strings.TrimSpace(out) != "ok" {
		t.Errorf("exec in a sandbox from the template printed %q", out)
	}
	sbx(t, "rm", "--force", sandbox)

	files[dockerfilePath] = from + "RUN true # changed\n"
	second, ok, err := e.Template(ctx)
	if err != nil || !ok || second == first {
		t.Fatalf("Template after a change = %q, %v, %v", second, ok, err)
	}
	wantTags(t, e, second)

	files[dockerfilePath] = from + "RUN echo exomecha-test-output && false\n"
	_, _, err = e.Template(ctx)
	p, ok := errors.AsType[*Problem](err)
	if !ok {
		t.Fatalf("Template with RUN false = %v, want a Problem", err)
	}
	if !strings.Contains(p.Output, "exomecha-test-output") || strings.Contains(p.Error(), "exomecha-test-output") {
		t.Errorf("Problem = %q with output %q, want the output only in Output", p.Error(), p.Output)
	}
	t.Logf("build failure: %s\noutput tail:\n%s", p.Why(), p.Output[max(0, len(p.Output)-600):])
	wantTags(t, e, second)
	if exists, err := e.sandboxExists(ctx, e.helper()); err != nil || exists {
		t.Errorf("helper left after a failed build: %v, %v", exists, err)
	}
}

func sbx(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("sbx", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("sbx %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func wantTags(t *testing.T, e *Env, ref string) {
	t.Helper()
	images, err := e.templates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, img := range images {
		if img.Repository == e.repository() {
			tags = append(tags, img.Repository+":"+img.Tag)
		}
	}
	if len(tags) != 1 || tags[0] != ref {
		t.Errorf("templates of the project = %q, want only %q", tags, ref)
	}
}

func cleanUp(t *testing.T, e *Env, sandbox string) {
	ctx := context.Background()
	for _, name := range []string{sandbox, e.helper()} {
		if err := e.removeSandbox(ctx, name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	}
	images, err := e.templates(ctx)
	if err != nil {
		t.Errorf("list templates: %v", err)
		return
	}
	for _, img := range images {
		if img.Repository == e.repository() {
			if err := e.sbx(ctx, "template", "rm", "--force", img.Repository+":"+img.Tag); err != nil {
				t.Errorf("remove template: %v", err)
			}
		}
	}
}
