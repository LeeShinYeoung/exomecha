package template

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/LeeShinYeoung/exomecha/internal/sandbox"
)

// Real sbx behavior, such as loading from /dev/stdin, cannot be faked. Takes a few minutes.
func TestRealBuild(t *testing.T) {
	if os.Getenv("EXOMECHA_TEST_SBX") != "1" {
		t.Skip("set EXOMECHA_TEST_SBX=1 to build templates with the real sbx")
	}
	const from = "FROM docker/sandbox-templates:claude-code-docker\n"
	const box = "impl-env-from-template"
	files := fakeFiles{
		dockerfilePath: from + "USER root\nRUN echo from-template > /etc/exomecha-template\nUSER agent\n",
		hostsPath:      "example.com # proves the helper takes allowed-hosts\n",
	}
	e := New(files, sandbox.CLI{}, "impl-env", "realtest")
	t.Cleanup(func() { cleanUp(t, e, box) })
	ctx := t.Context()

	first, err := e.Template(ctx)
	if err != nil || first != e.ref(files[dockerfilePath]) {
		t.Fatalf("Template = %q, %v", first, err)
	}
	wantTags(t, e, first)
	if listed(t, e.helper) {
		t.Error("helper left after a build")
	}
	if again, err := e.Template(ctx); err != nil || again != first {
		t.Errorf("Template again = %q, %v", again, err)
	}

	if err := (sandbox.CLI{}).Create(ctx, box, first, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if out := sbx(t, "exec", box, "cat", "/etc/exomecha-template"); strings.TrimSpace(out) != "from-template" {
		t.Errorf("the file the Dockerfile wrote, in a sandbox from the template = %q", out)
	}
	sbx(t, "rm", "--force", box)

	files[dockerfilePath] = from + "RUN true # changed\n"
	second, err := e.Template(ctx)
	if err != nil || second == "" || second == first {
		t.Fatalf("Template after a change = %q, %v", second, err)
	}
	wantTags(t, e, second)

	files[dockerfilePath] = from + "RUN echo exomecha-test-output && false\n"
	_, err = e.Template(ctx)
	p, ok := errors.AsType[*Problem](err)
	if !ok {
		t.Fatalf("Template with RUN false = %v, want a Problem", err)
	}
	if !strings.Contains(p.Output(), "exomecha-test-output") || strings.Contains(p.Error(), "exomecha-test-output") {
		t.Errorf("Problem = %q with output %q, want the output only in Output", p.Error(), p.Output())
	}
	t.Logf("build failure: %s\noutput tail:\n%s", p.Why(), p.Output()[max(0, len(p.Output())-600):])
	wantTags(t, e, second)
	if listed(t, e.helper) {
		t.Error("helper left after a failed build")
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

func listed(t *testing.T, name string) bool {
	t.Helper()
	boxes, err := sandbox.CLI{}.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(boxes, func(b sandbox.Box) bool { return b.Name == name })
}

func wantTags(t *testing.T, e *Env, ref string) {
	t.Helper()
	images, err := e.sbx.Templates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, img := range images {
		if img.Repository == e.repository() {
			tags = append(tags, img.Ref())
		}
	}
	if len(tags) != 1 || tags[0] != ref {
		t.Errorf("templates of the project = %q, want only %q", tags, ref)
	}
}

func cleanUp(t *testing.T, e *Env, box string) {
	ctx := context.Background()
	for _, name := range []string{box, e.helper} {
		if err := e.sbx.Remove(ctx, name); err != nil {
			t.Errorf("remove %s: %v", name, err)
		}
	}
	images, err := e.sbx.Templates(ctx)
	if err != nil {
		t.Errorf("list templates: %v", err)
		return
	}
	for _, img := range images {
		if img.Repository == e.repository() {
			if err := e.sbx.RemoveTemplate(ctx, img.Ref()); err != nil {
				t.Errorf("remove template: %v", err)
			}
		}
	}
}
