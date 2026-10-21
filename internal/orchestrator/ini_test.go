package orchestrator

import (
	"strings"
	"testing"

	"ts3panel/internal/model"
)

func TestRenderINIExactContent(t *testing.T) {
	data, err := RenderINI(model.Ports{Voice: 9987, FileTransfer: 30033, Query: 10011, QuerySSH: 10022})
	if err != nil {
		t.Fatalf("RenderINI: %v", err)
	}
	want := `voice_ip=0.0.0.0
filetransfer_ip=0.0.0.0
query_ip=127.0.0.1

default_voice_port=9987
filetransfer_port=30033
query_port=10011
query_ssh_port=10022

dbplugin=ts3db_sqlite3
dbsqlite3_path=/var/ts3server/
logpath=/var/ts3server/logs/
licensepath=
`
	if string(data) != want {
		t.Fatalf("unexpected ini:\n%s", data)
	}
}

func TestRenderINIKeepsQueryOnLoopback(t *testing.T) {
	data, err := RenderINI(model.Ports{Voice: 9988, FileTransfer: 30034, Query: 10012, QuerySSH: 10023})
	if err != nil {
		t.Fatalf("RenderINI: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "query_ip=127.0.0.1\n") {
		t.Fatalf("query_ip is not loopback:\n%s", text)
	}
	if strings.Contains(text, "0.0.0.0") && strings.Count(text, "0.0.0.0") != 2 {
		t.Fatalf("unexpected wildcard bindings:\n%s", text)
	}
	if strings.Contains(text, "serveradmin_password") {
		t.Fatalf("serveradmin_password must not be written:\n%s", text)
	}
	if !strings.Contains(text, "default_voice_port=9988") || !strings.Contains(text, "query_ssh_port=10023") {
		t.Fatalf("ports missing:\n%s", text)
	}
}

func TestRenderINIRejectsBadPorts(t *testing.T) {
	cases := []model.Ports{
		{Voice: 0, FileTransfer: 30033, Query: 10011, QuerySSH: 10022},
		{Voice: 9987, FileTransfer: 70000, Query: 10011, QuerySSH: 10022},
		{Voice: 9987, FileTransfer: 30033, Query: -1, QuerySSH: 10022},
	}
	for _, ports := range cases {
		if _, err := RenderINI(ports); err == nil {
			t.Fatalf("RenderINI(%+v) accepted invalid ports", ports)
		}
	}
}
