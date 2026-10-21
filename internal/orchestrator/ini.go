package orchestrator

import (
	"errors"
	"fmt"

	"ts3panel/internal/model"
)

// iniTemplate is the exact server configuration from the specification §11.2.
// Query ports bind loopback only; the panel never enables ServerQuery (D6), so
// no serveradmin_password is written and TS3 keeps generating its own.
const iniTemplate = `voice_ip=0.0.0.0
filetransfer_ip=0.0.0.0
query_ip=127.0.0.1

default_voice_port=%d
filetransfer_port=%d
query_port=%d
query_ssh_port=%d

dbplugin=ts3db_sqlite3
dbsqlite3_path=/var/ts3server/
logpath=/var/ts3server/logs/
licensepath=
`

// RenderINI renders ts3server.ini for one port group. It refuses zero or
// out-of-range ports so a bad allocation can never produce a half-working file.
// Plain string formatting is used deliberately: the project rules exclude
// template engines.
func RenderINI(ports model.Ports) ([]byte, error) {
	for name, port := range map[string]int{
		"voice":        ports.Voice,
		"filetransfer": ports.FileTransfer,
		"query":        ports.Query,
		"query_ssh":    ports.QuerySSH,
	} {
		if port < 1 || port > 65535 {
			return nil, errors.New("invalid " + name + " port")
		}
	}
	return []byte(fmt.Sprintf(iniTemplate, ports.Voice, ports.FileTransfer, ports.Query, ports.QuerySSH)), nil
}
