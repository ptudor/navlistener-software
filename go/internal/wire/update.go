package wire

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
)

// UpdateCommand is authenticated intent. The device independently checks TUF,
// eligibility, expiry, command replay and its durable observation watermark.
type UpdateCommand struct {
	Action     uint8  `json:"action"`
	Hint       bool   `json:"hint"`
	ID         uint64 `json:"command_id,string"`
	Generation uint64 `json:"channel_generation,string"`
	Release    uint64 `json:"release_sequence,string"`
	Expires    uint64 `json:"expires,string"`
}

func (c UpdateCommand) Valid() bool {
	return c.Action >= 1 && c.Action <= 4 && (!c.Hint || c.Action == 1) && c.ID != 0 && c.Expires != 0 &&
		((c.Action != 2 && c.Action != 3) || (c.Generation != 0 && c.Release != 0))
}
func (c UpdateCommand) Encode() ([]byte, error) {
	if !c.Valid() {
		return nil, errors.New("invalid update command")
	}
	p := make([]byte, 36)
	p[0] = 1
	p[1] = c.Action
	if c.Hint {
		p[3] = 1
	}
	for i, n := range []uint64{c.ID, c.Generation, c.Release, c.Expires} {
		binary.BigEndian.PutUint64(p[4+i*8:], n)
	}
	return p, nil
}
func DecodeUpdateCommand(p []byte) (UpdateCommand, error) {
	if len(p) != 36 || p[0] != 1 || p[2] != 0 || p[3] > 1 {
		return UpdateCommand{}, errors.New("invalid update command framing")
	}
	u := binary.BigEndian.Uint64
	c := UpdateCommand{Action: p[1], Hint: p[3] == 1, ID: u(p[4:]), Generation: u(p[12:]), Release: u(p[20:]), Expires: u(p[28:])}
	if !c.Valid() {
		return UpdateCommand{}, errors.New("invalid update command fields")
	}
	return c, nil
}

type UpdateStatus struct {
	Mode        string `json:"mode"`
	Channel     string `json:"channel"`
	State       string `json:"state"`
	Security    uint8  `json:"security_flags"`
	Layout      uint16 `json:"partition_layout_id"`
	Running     uint64 `json:"running_release,string"`
	Available   uint64 `json:"available_release,string"`
	Staged      uint64 `json:"staged_release,string"`
	Failed      uint64 `json:"failed_release,string"`
	Received    uint32 `json:"bytes_received"`
	Total       uint32 `json:"artifact_length"`
	LastCheck   uint64 `json:"last_check,string"`
	NextCheck   uint64 `json:"next_check,string"`
	LastCommand uint64 `json:"last_command,string"`
	ErrorDomain uint16 `json:"error_domain"`
	ErrorReason uint16 `json:"error_reason"`
	Error       string `json:"error"`
	BootKey     string `json:"secure_boot_key_id"`
	ReleaseKey  string `json:"tuf_release_key_id"`
}

func DecodeUpdateStatus(p []byte) (*UpdateStatus, error) {
	if len(p) != 140 || p[0] != 1 || p[1] < 1 || p[1] > 3 || p[2] < 1 || p[2] > 3 || p[3] > 11 || p[4]&^31 != 0 || p[5] != 0 {
		return nil, errors.New("invalid update status framing")
	}
	u16, u32, u64 := binary.BigEndian.Uint16, binary.BigEndian.Uint32, binary.BigEndian.Uint64
	if u16(p[72:]) > 7 || (u16(p[72:]) == 0) != (u16(p[74:]) == 0) || u32(p[44:]) > 0x200000 || u32(p[40:]) > u32(p[44:]) {
		return nil, errors.New("invalid update status fields")
	}
	s := &UpdateStatus{Mode: []string{"manual", "download", "install"}[p[1]-1], Channel: []string{"stable", "canary", "lab"}[p[2]-1],
		State:    []string{"idle", "checking", "available", "downloading", "staged", "waiting-safe", "quiescing", "reboot-pending", "trial-boot", "confirmed", "rolled-back", "failed"}[p[3]],
		Security: p[4], Layout: u16(p[6:]), Running: u64(p[8:]), Available: u64(p[16:]), Staged: u64(p[24:]), Failed: u64(p[32:]),
		Received: u32(p[40:]), Total: u32(p[44:]), LastCheck: u64(p[48:]), NextCheck: u64(p[56:]), LastCommand: u64(p[64:]),
		ErrorDomain: u16(p[72:]), ErrorReason: u16(p[74:]), BootKey: hex.EncodeToString(p[76:108]), ReleaseKey: hex.EncodeToString(p[108:140])}
	s.Error = UpdateErrorName(s.ErrorDomain, s.ErrorReason)
	return s, nil
}
