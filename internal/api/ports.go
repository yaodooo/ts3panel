package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"ts3panel/internal/model"
)

// Port checklist endpoint. It is deliberately read-only and derived from
// state.json (not from Docker): the ports are allocated before the container
// exists, and a stopped or missing container still owns its port group.
const (
	portProtocolUDP = "UDP"
	portProtocolTCP = "TCP"

	portPurposeVoice        = "语音"
	portPurposeFileTransfer = "文件传输"
	portPurposeQuery        = "ServerQuery"
	portPurposeQuerySSH     = "SSH 查询"
)

// portRange is one merged, inclusive interval shown in the checklist.
type portRange struct {
	Protocol string `json:"protocol"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
	Purpose  string `json:"purpose"`
	Display  string `json:"display"`
}

// portsResponse is the payload behind GET /api/ports. Text is the exact,
// copy-ready block so the browser never has to reconstruct the wording.
type portsResponse struct {
	InstanceCount int         `json:"instance_count"`
	Public        []portRange `json:"public"`
	DoNotOpen     []portRange `json:"do_not_open"`
	Text          string      `json:"text"`
}

type portNeed struct {
	protocol string
	port     int
	purpose  string
}

// handlePorts aggregates the four ports of every instance into public
// (UDP voice, TCP filetransfer) and internal (the two loopback-only query
// ports) ranges. Contiguous ports are merged per protocol; gaps stay separate.
func (a *API) handlePorts(w http.ResponseWriter, r *http.Request) {
	st, err := a.state.Snapshot()
	if err != nil {
		a.internalError(w, "port checklist", err)
		return
	}
	public, internal := collectPortRanges(st.Instances)
	writeJSON(w, http.StatusOK, portsResponse{
		InstanceCount: len(st.Instances),
		Public:        public,
		DoNotOpen:     internal,
		Text:          renderPortsText(public, internal),
	})
}

func collectPortRanges(instances []model.Instance) (public, internal []portRange) {
	var pubNeeds, intNeeds []portNeed
	for _, inst := range instances {
		if inst.Ports.Voice > 0 {
			pubNeeds = append(pubNeeds, portNeed{portProtocolUDP, inst.Ports.Voice, portPurposeVoice})
		}
		if inst.Ports.FileTransfer > 0 {
			pubNeeds = append(pubNeeds, portNeed{portProtocolTCP, inst.Ports.FileTransfer, portPurposeFileTransfer})
		}
		if inst.Ports.Query > 0 {
			intNeeds = append(intNeeds, portNeed{portProtocolTCP, inst.Ports.Query, portPurposeQuery})
		}
		if inst.Ports.QuerySSH > 0 {
			intNeeds = append(intNeeds, portNeed{portProtocolTCP, inst.Ports.QuerySSH, portPurposeQuerySSH})
		}
	}
	return mergePortNeeds(pubNeeds), mergePortNeeds(intNeeds)
}

func mergePortNeeds(needs []portNeed) []portRange {
	if len(needs) == 0 {
		return []portRange{}
	}

	// Deduplicate defensively: state.json is operator-editable and duplicate
	// groups must not produce duplicate checklist rows.
	seen := make(map[string]struct{}, len(needs))
	unique := make([]portNeed, 0, len(needs))
	for _, need := range needs {
		key := fmt.Sprintf("%s/%d", need.protocol, need.port)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, need)
	}

	sort.Slice(unique, func(i, j int) bool {
		if unique[i].protocol != unique[j].protocol {
			return protocolOrder(unique[i].protocol) < protocolOrder(unique[j].protocol)
		}
		return unique[i].port < unique[j].port
	})

	out := make([]portRange, 0, len(unique))
	for _, need := range unique {
		last := len(out) - 1
		if last >= 0 && out[last].Protocol == need.protocol &&
			out[last].Purpose == need.purpose && need.port == out[last].End+1 {
			out[last].End = need.port
			out[last].Display = rangeDisplay(need.protocol, out[last].Start, out[last].End)
			continue
		}
		out = append(out, portRange{
			Protocol: need.protocol,
			Start:    need.port,
			End:      need.port,
			Purpose:  need.purpose,
			Display:  rangeDisplay(need.protocol, need.port, need.port),
		})
	}
	return out
}

func protocolOrder(protocol string) int {
	if protocol == portProtocolUDP {
		return 0
	}
	return 1
}

func rangeDisplay(protocol string, start, end int) string {
	if start == end {
		return fmt.Sprintf("%s %d", protocol, start)
	}
	return fmt.Sprintf("%s %d-%d", protocol, start, end)
}

func internalPortList(ranges []portRange) string {
	if len(ranges) == 0 {
		return ""
	}
	parts := make([]string, 0, len(ranges))
	for _, item := range ranges {
		if item.Start == item.End {
			parts = append(parts, fmt.Sprintf("%d", item.Start))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d-%d", item.Start, item.End))
	}
	return strings.Join(parts, "、")
}

// renderPortsText produces the block the operator can paste into a ticket.
// Wording follows the spec's §10.3 example: public rules first, then the
// loopback-only query ports that must stay closed.
func renderPortsText(public, internal []portRange) string {
	var b strings.Builder
	if len(public) == 0 {
		b.WriteString("当前没有实例，无需开放端口。\n")
	} else {
		b.WriteString("需要开放的安全组入方向：\n")
		for _, item := range public {
			b.WriteString(fmt.Sprintf("%-16s %s（全部实例）\n", item.Display, item.Purpose))
		}
	}
	if list := internalPortList(internal); list != "" {
		b.WriteString("\n不要开放：")
		b.WriteString(portProtocolTCP)
		b.WriteByte(' ')
		b.WriteString(list)
		b.WriteString("（查询端口，仅监听 127.0.0.1）\n")
	}
	return b.String()
}
