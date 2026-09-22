package store

import "time"

// ReceiverConfigLog is the audit record for one receiver-key change.
type ReceiverConfigLog struct {
	ID                                              int64
	Actor, KeyID, KeyName, OldValue, NewValue, Note string
	Verified, Reverted                              bool
}

func (s *Store) LogReceiverConfig(v ReceiverConfigLog) (int64, error) {
	r, err := s.db.Exec(`INSERT INTO receiver_config_log
 (ts,actor,key_id,key_name,old_value,new_value,verified,reverted,note)
 VALUES (?,?,?,?,?,?,?,?,?)`, time.Now().Unix(), v.Actor, v.KeyID, v.KeyName,
		v.OldValue, v.NewValue, boolInt(v.Verified), boolInt(v.Reverted), v.Note)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) MarkReceiverConfigReverted(id int64) error {
	_, err := s.db.Exec(`UPDATE receiver_config_log SET reverted=1 WHERE id=?`, id)
	return err
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
