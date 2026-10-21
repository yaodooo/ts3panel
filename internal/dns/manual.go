package dns

import "context"

// manual is the "no API" provider from spec §12.4. It never writes records: the
// panel renders a copyable checklist and the operator adds the records in the
// DNS console. Deletion is deliberately a no-op because the panel does not own
// records it did not create.
type manual struct {
	zone string
}

func newManual(zone string) *manual {
	return &manual{zone: normalizeName(zone)}
}

// Validate performs no network I/O; it reports the configured zone so the
// settings page can show what the checklist will target.
func (m *manual) Validate(context.Context) ([]Zone, error) {
	if m.zone == "" {
		return nil, Errorf(CodeZone, "manual provider requires a zone")
	}
	return []Zone{{Name: m.zone}}, nil
}

func (m *manual) UpsertA(context.Context, string, string, string, int) (RecordID, error) {
	return "", ErrManualMode
}

func (m *manual) UpsertSRV(context.Context, string, string, int, int) (RecordID, error) {
	return "", ErrManualMode
}

func (m *manual) DeleteRecord(context.Context, string, RecordID) error {
	return nil
}

// IsManual reports whether p is the manual provider. The API uses it to skip
// writing and verification and to return the checklist instead.
func IsManual(p Provider) bool {
	_, ok := p.(*manual)
	return ok
}

// Records returns the A + SRV checklist for one instance.
func (m *manual) Records(fqdn, ip string, port, ttl int) ([]ManualRecord, error) {
	return ManualRecords(m.zone, fqdn, ip, port, ttl)
}

var _ Provider = (*manual)(nil)
