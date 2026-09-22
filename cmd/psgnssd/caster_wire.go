package main

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/psgnss/psgnss-base/internal/caster"
	"github.com/psgnss/psgnss-base/internal/config"
	"github.com/psgnss/psgnss-base/internal/ephemeris"
	"github.com/psgnss/psgnss-base/internal/hub"
	"github.com/psgnss/psgnss-base/internal/rtcm"
	"github.com/psgnss/psgnss-base/internal/secrets"
	"github.com/psgnss/psgnss-base/internal/store"
)

// buildCaster assembles the caster and its mountpoints from config.
func buildCaster(cfg *config.Config, h *hub.Hub, db *store.Store,
	kr *secrets.Keyring, eph *ephemeris.Store, log *slog.Logger) (*caster.Caster, error) {

	trust, err := caster.NewTrustList(cfg.Caster.ProxyTrusted)
	if err != nil {
		return nil, err
	}
	proxyListen := cfg.Caster.ProxyListen
	if len(cfg.Caster.ProxyTrusted) == 0 {
		// A PROXY listener with nothing trusted would accept headers from
		// nobody and quietly behave like the plain one. Leave it off instead of
		// pretending it works.
		if proxyListen != "" {
			log.Info("proxy-protocol listener disabled: caster.proxy_trusted is empty",
				"would_listen", proxyListen)
		}
		proxyListen = ""
	}

	// Station values for the synthesised 1006/1008/1033.
	pos := cfg.Station.Position
	x, y, z := rtcm.LLHToECEF(pos.Latitude, pos.Longitude, pos.Height)
	station := rtcm.Station{
		ID: cfg.Station.StationID, X: x, Y: y, Z: z,
		AntennaHeight:     pos.ENUOffset[2],
		AntennaDescriptor: cfg.Station.Antenna,
		ReceiverType:      cfg.Station.Receiver,
		GPS:               true,
		Galileo:           true,
	}

	c, err := caster.New(caster.Options{
		Listen:      cfg.Caster.Listen,
		ProxyListen: proxyListen,
		Trusted:     trust,
		AllowV1:     cfg.Caster.NtripV1,
		AllowV2:     cfg.Caster.NtripV2,
		Net: &caster.NetEntry{
			Network: "PSGNSS", Operator: cfg.Caster.Operator, Auth: "B", Fee: "N",
		},
		Store:     db,
		Keyring:   kr,
		Station:   station,
		Ephemeris: eph,
		Hub:       h,
		Logger:    log,
	})
	if err != nil {
		return nil, err
	}

	for _, mp := range cfg.Caster.Mountpoint {
		if mp.Disabled {
			continue
		}
		specs := make([]hub.FilterSpec, 0, len(mp.Messages))
		types := make([]int, 0, len(mp.Messages))
		intervals := map[int]int{}
		generated := map[int]int{}
		for _, m := range mp.Messages {
			types = append(types, m.Type)
			intervals[m.Type] = m.Interval
			// Types the receiver never emits are synthesised rather than
			// filtered from a stream that will never contain them.
			if rtcm.CanGenerate(m.Type) || ephemeris.Supports(m.Type) {
				generated[m.Type] = m.Interval
				continue
			}
			specs = append(specs, hub.FilterSpec{Type: m.Type, Interval: m.Interval})
		}
		f, err := hub.NewFilter(hub.ProtoRTCM3, specs)
		if err != nil {
			return nil, fmt.Errorf("mountpoint %q: %w", mp.Name, err)
		}
		format := mp.Format
		if format == "" {
			format = cfg.Caster.FormatString
		}
		carrier := cfg.Caster.Carrier
		if mp.Carrier != nil {
			carrier = *mp.Carrier
		}
		network := mp.Network
		if network == "" {
			network = cfg.Caster.Operator
		}
		country := mp.Country
		if country == "" {
			country = cfg.Caster.Country
		}
		generator := mp.Generator
		if generator == "" {
			generator = "PSGNSS"
		}
		auth := mp.Auth
		if auth == "" {
			auth = "B"
		}
		fee := mp.Fee
		if fee == "" {
			fee = "N"
		}
		mount := c.AddMount(caster.MountEntry{
			Name:      mp.Name,
			SourceID:  mp.SourceID,
			Format:    format,
			Messages:  caster.FormatMessages(types, intervals),
			Carrier:   carrier,
			NavSystem: mp.NavSystem,
			Network:   network,
			Country:   country,
			Latitude:  cfg.Station.Position.Latitude,
			Longitude: cfg.Station.Position.Longitude,
			NMEA:      mp.NMEA,
			Solution:  mp.Solution,
			Generator: generator,
			Compress:  mp.Compress,
			Auth:      auth,
			Fee:       fee,
			Bitrate:   mp.Bitrate,
			MSMDetail: mp.MSMDetail,
		}, f)
		mount.Generated = generated
		mount.Terminator = rtcm.EpochTerminator(types)
		if mount.Terminator != 0 {
			log.Info("mountpoint epoch terminator",
				"mount", mp.Name, "type", mount.Terminator)
		}
		if len(generated) > 0 {
			log.Info("mountpoint will synthesise station messages",
				"mount", mp.Name, "types", sortedTypes(generated))
		}
	}
	return c, nil
}

func sortedTypes(m map[int]int) []int {
	out := make([]int, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Ints(out)
	return out
}

// --- user management CLI ---

func userAdd(cfg *config.Config, username, password string, limit int) error {
	db, kr, err := openStoreAndKey(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	u, err := db.CreateUser(kr, username, password, limit, "")
	if err != nil {
		return err
	}
	fmt.Printf("created user %q (id %d, connection limit %d)\n", u.Username, u.ID, u.ConnectionLimit)
	return nil
}

func userList(cfg *config.Config, showPasswords bool) error {
	db, kr, err := openStoreAndKey(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	users, err := db.ListUsers()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("no users")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	hdr := "USERNAME\tLIMIT\tENABLED\tACTIVE\tCREATED"
	if showPasswords {
		hdr += "\tPASSWORD"
	}
	fmt.Fprintln(w, hdr)
	for _, u := range users {
		active, _ := db.ActiveConnections(u.Username)
		row := fmt.Sprintf("%s\t%d\t%v\t%d\t%s", u.Username, u.ConnectionLimit,
			u.Enabled, active, u.CreatedAt.Format("2006-01-02"))
		if showPasswords {
			p, err := db.GetPassword(kr, u.Username)
			if err != nil {
				p = "<decrypt failed>"
			}
			row += "\t" + p
		}
		fmt.Fprintln(w, row)
	}
	return w.Flush()
}

func connList(cfg *config.Config, n int) error {
	db, _, err := openStoreAndKey(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	conns, err := db.RecentConnections(n)
	if err != nil {
		return err
	}
	if len(conns) == 0 {
		fmt.Println("no connections recorded")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "USER\tMOUNTPOINT\tCLIENT IP\tVIA PROXY\tSTARTED\tDURATION\tBYTES\tAGENT")
	for _, c := range conns {
		dur := "active"
		if c.EndedAt != nil {
			dur = c.EndedAt.Sub(c.StartedAt).Round(time.Second).String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%v\t%s\t%s\t%d\t%s\n",
			c.Username, c.Mountpoint, c.ClientIP, c.ViaProxy,
			c.StartedAt.Format("2006-01-02 15:04:05"), dur, c.BytesSent, c.UserAgent)
	}
	return w.Flush()
}

func openStoreAndKey(cfg *config.Config) (*store.Store, *secrets.Keyring, error) {
	db, err := store.Open(cfg.Telemetry.DBPath)
	if err != nil {
		return nil, nil, err
	}
	kr, created, err := secrets.LoadOrGenerate(cfg.Security.KeyFile)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	if created {
		fmt.Fprintf(os.Stderr, "generated new master key at %s (mode 0600)\n", cfg.Security.KeyFile)
	}
	return db, kr, nil
}

// adminCreate makes the single web administrator account.
func adminCreate(cfg *config.Config, username, password string) error {
	db, _, err := openStoreAndKey(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.CreateAdmin(username, password); err != nil {
		// Creating an existing admin should change the password, not fail with
		// a constraint error the operator has to decode.
		if err2 := db.SetAdminPassword(username, password); err2 != nil {
			return err
		}
		fmt.Printf("updated password for existing admin %q\n", username)
		return nil
	}
	fmt.Printf("created admin %q (password is hashed with argon2id, not recoverable)\n", username)
	return nil
}
