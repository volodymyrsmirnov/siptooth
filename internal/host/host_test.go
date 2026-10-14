package host

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDoPrintsThenRuns(t *testing.T) {
	f := &FakeRunner{}
	var out bytes.Buffer
	if err := Do(context.Background(), f, &out, Cmd{Argv: []string{"tee", "/x y"}, Stdin: []byte("data"), Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "$ tee '/x y'\n" {
		t.Fatalf("%q", out.String())
	}
	if len(f.Cmds) != 1 || string(f.Cmds[0].Stdin) != "data" {
		t.Fatalf("%+v", f.Cmds)
	}
	f.Err = errors.New("boom")
	if err := Do(context.Background(), f, &out, Cmd{Argv: []string{"false"}}); err == nil || !strings.Contains(err.Error(), "false") {
		t.Fatalf("%v", err)
	}
}

func TestFakeRunnerFailsSelectedCommands(t *testing.T) {
	f := &FakeRunner{Fail: map[string]error{"loginctl enable-linger pi": errors.New("denied")}}
	if err := f.Run(context.Background(), Cmd{Argv: []string{"systemctl", "--user", "daemon-reload"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Run(context.Background(), Cmd{Argv: []string{"loginctl", "enable-linger", "pi"}}); err == nil || err.Error() != "denied" {
		t.Fatalf("%v", err)
	}
	if len(f.Cmds) != 2 {
		t.Fatalf("%+v", f.Cmds)
	}
}

var params = UnitParams{ConfigPath: "/home/pi/.siptooth/config.yaml", Executable: "/home/pi/.local/bin/siptooth"}

func TestUserUnitPath(t *testing.T) {
	if p := UserUnitPath("/home/pi", ""); p != "/home/pi/.config/systemd/user/siptooth.service" {
		t.Fatal(p)
	}
	if p := UserUnitPath("/home/pi", "/xdg"); p != "/xdg/systemd/user/siptooth.service" {
		t.Fatal(p)
	}
}

func TestUnitContent(t *testing.T) {
	u := Unit(params)
	want := `[Unit]
Description=Siptooth phone-to-SIP bridge

[Service]
WorkingDirectory=/home/pi/.siptooth
ExecStart=/home/pi/.local/bin/siptooth run --config /home/pi/.siptooth/config.yaml
Restart=on-failure
RestartSec=3
RestartPreventExitStatus=2

[Install]
WantedBy=default.target
`
	if u != want {
		t.Fatalf("unit:\n%s", u)
	}
}

func TestUnitQuotesPathsWithSpaces(t *testing.T) {
	p := params
	p.ConfigPath = "/home/pi/my dir/config.yaml"
	u := Unit(p)
	if !strings.Contains(u, `run --config "/home/pi/my dir/config.yaml"`) || !strings.Contains(u, `WorkingDirectory=/home/pi/my dir`) {
		t.Fatal(u)
	}
	if got := ParseUnit(u); got.Config != "/home/pi/my dir/config.yaml" {
		t.Fatalf("%+v", got)
	}
}

func TestParseUnit(t *testing.T) {
	got := ParseUnit(Unit(params))
	if got.Executable != "/home/pi/.local/bin/siptooth" || got.Config != "/home/pi/.siptooth/config.yaml" || !got.SetsExecStart {
		t.Fatalf("%+v", got)
	}
	got = ParseUnit("[Service]\n# ExecStart=/nope\nExecStart=/usr/bin/siptooth run --config /c.yaml\n")
	if got.Executable != "/usr/bin/siptooth" || got.Config != "/c.yaml" {
		t.Fatalf("%+v", got)
	}
	if got := ParseUnit("ExecStart=-/bin/x run\n"); got.Executable != "/bin/x" {
		t.Fatalf("prefix: %+v", got)
	}
	if got := ParseUnit("[Service]\nRestart=always\n"); got.SetsExecStart {
		t.Fatalf("%+v", got)
	}
}

func TestInstallCommands(t *testing.T) {
	cmds := InstallCommands("pi")
	var lines []string
	for _, c := range cmds {
		lines = append(lines, c.String())
		if strings.Contains(c.String(), "sudo") {
			t.Fatalf("no sudo: %s", c)
		}
	}
	want := "systemctl --user daemon-reload|systemctl --user enable siptooth|systemctl --user restart siptooth|loginctl enable-linger pi"
	if strings.Join(lines, "|") != want {
		t.Fatalf("%q", strings.Join(lines, "|"))
	}
	for i, c := range cmds {
		if c.Optional != (i == 3) {
			t.Fatalf("only enable-linger is optional: %+v", cmds)
		}
	}
}
