package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/psgnss/psgnss-base/internal/boardprofile"
	"github.com/psgnss/psgnss-base/internal/setup"
)

// runSetup is the first-run interview. It asks only what cannot be defaulted,
// shows the whole plan, and writes nothing until the operator agrees.
//
// Every prompt carries a default in brackets, and pressing enter takes it.
// Someone who has just flashed a card and knows only where their antenna is
// can hold enter through most of this.
func runSetup(root string, dryRun, force, assumeYes bool) error {
	in := bufio.NewReader(os.Stdin)
	paths := setup.DefaultPaths()
	a := setup.Defaults()

	if setup.IsInstalled(root, paths) && !force {
		// These are printed to the operator as-is rather than wrapped into
		// another error, so the sentences and the line breaks are the message.
		//lint:ignore ST1005 operator-facing guidance, printed verbatim
		return fmt.Errorf("%w: %s exists.\n\n"+
			"Nothing has been changed. To reconfigure a working station use the Settings\n"+
			"page, which edits one setting at a time and keeps the database and the master\n"+
			"key. To genuinely rebuild this station from scratch, back up %s and\n"+
			"%s first, then re-run with --force.",
			setup.ErrAlreadyInstalled, paths.ConfigFile(), paths.MasterKey(), paths.Database())
	}

	fmt.Println()
	fmt.Println("PSGNSS first-run setup")
	fmt.Println("Enter takes the value in brackets. Ctrl-C stops without changing anything.")
	fmt.Println()

	// --- the receiver -----------------------------------------------------
	section("Receiver")
	fmt.Println("Supported boards:")
	for _, p := range boardprofile.All() {
		state := "no driver yet"
		if p.Implemented {
			state = "full support"
		}
		fmt.Printf("  %-22s %-12s %s\n", p.ID, p.Receiver, state)
	}
	fmt.Println()
	for {
		a.ProfileID = ask(in, "Board", a.ProfileID)
		p, ok := boardprofile.Lookup(a.ProfileID)
		if !ok {
			fmt.Println("  Not one of the boards above.")
			continue
		}
		if !p.Implemented {
			fmt.Printf("  %s (%s) has a profile but no driver in this release: PSGNSS could not\n"+
				"  configure or read it, so the station would not work. Pick a supported board.\n",
				p.Name, p.Receiver)
			continue
		}
		break
	}

	devices := setup.FindSerialDevices(root)
	if len(devices) == 0 {
		fmt.Println("No serial devices found. Plug the receiver in and it will appear under")
		fmt.Println("/dev/serial/by-id; you can also type a path now and connect it later.")
	} else {
		fmt.Println("Serial devices:")
		for i, d := range devices {
			mark := " "
			if d.Likely {
				mark = "*"
			}
			fmt.Printf(" %s %d) %s\n", mark, i+1, d.Path)
		}
		fmt.Println("   (* looks like a GNSS receiver. Always prefer a by-id path: /dev/ttyACM0")
		fmt.Println("   moves to whichever device enumerates first.)")
		a.Device = devices[0].Path
	}
	for {
		answer := ask(in, "Receiver device", a.Device)
		if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(devices) {
			answer = devices[n-1].Path
		}
		if strings.HasPrefix(answer, "/dev/") {
			a.Device = answer
			break
		}
		fmt.Println("  Needs to be a /dev path, or the number of one listed above.")
	}
	a.Baud = askInt(in, "Baud", a.Baud)

	// --- the station ------------------------------------------------------
	section("Station")
	for {
		a.StationName = ask(in, "Station name", a.StationName)
		if strings.TrimSpace(a.StationName) != "" {
			break
		}
		fmt.Println("  Required: it names the mountpoints and appears in the sourcetable.")
	}
	a.MSM7Name = a.StationName + "_MSM7"
	a.MSM4Name = a.StationName + "_MSM4"

	fmt.Println()
	fmt.Println("Reference station ID is broadcast in every message. It must match what the")
	fmt.Println("receiver stamps into its own observations: when the two disagree a rover")
	fmt.Println("receives a healthy-looking stream, tracks satellites and never fixes. This")
	fmt.Println("receiver family stamps 0 out of the box, so 0 is the answer unless you are")
	fmt.Println("also writing CFG-RTCM DF003 on the receiver.")
	a.StationID = uint16(askInt(in, "Station ID", int(a.StationID)))
	a.Antenna = ask(in, "Antenna descriptor (RTCM 1008)", a.Antenna)

	fmt.Println()
	fmt.Println("Base position. This is broadcast as the station's coordinate and every rover")
	fmt.Println("fixes relative to it: an error here moves every measured point by the same")
	fmt.Println("amount, silently. Use a surveyed coordinate, not one the receiver averaged.")
	a.Position.Latitude = askFloat(in, "Latitude (degrees)", a.Position.Latitude)
	a.Position.Longitude = askFloat(in, "Longitude (degrees)", a.Position.Longitude)
	a.Position.Height = askFloat(in, "Ellipsoidal height (m)", a.Position.Height)

	// --- what it serves ---------------------------------------------------
	section("Mountpoints and ports")
	a.MSM7Name = ask(in, "MSM7 mountpoint (full resolution)", a.MSM7Name)
	a.MSM4Name = ask(in, "MSM4 mountpoint (compact)", a.MSM4Name)
	fmt.Println()
	fmt.Println("This receiver cannot emit RTCM ephemeris, so PSGNSS can synthesise 1019,")
	fmt.Println("1042 and 1046 from the raw navigation subframes. A rover that already holds")
	fmt.Println("an ephemeris ignores them; one that does not starts faster. About 0.5 kbit/s.")
	a.Ephemeris = askYesNo(in, "Broadcast ephemeris", a.Ephemeris)
	a.CasterListen = ask(in, "NTRIP caster listens on", a.CasterListen)
	a.WebListen = ask(in, "Web UI listens on", a.WebListen)
	a.Operator = ask(in, "Operator (sourcetable)", a.Operator)
	a.Country = strings.ToUpper(ask(in, "Country (3 letters)", a.Country))

	// --- the archive ------------------------------------------------------
	section("Raw archive")
	fmt.Println("Raw data is always recorded to the local spool. It can also be flushed to an")
	fmt.Println("SMB share, which is what makes it survive the card. Without one, the station")
	fmt.Println("records to the Pi only and you can add a share later.")
	a.Archive.Enabled = askYesNo(in, "Flush the archive to an SMB share", false)
	if a.Archive.Enabled {
		a.Archive.UNC = ask(in, "Share (//host/share)", a.Archive.UNC)
		a.Archive.MountPoint = ask(in, "Mount it at", a.Archive.MountPoint)
		a.Archive.Username = ask(in, "Share username", a.Archive.Username)
		a.Archive.Password = askSecret(in, "Share password")
		a.Archive.RetentionDays = askInt(in, "Days of archive to keep (0 = never prune)", a.Archive.RetentionDays)
	}

	// --- the administrator ------------------------------------------------
	section("Administrator")
	fmt.Println("This account signs in to the web UI. The password is hashed and cannot be")
	fmt.Println("read back, here or in the UI.")
	a.AdminUser = ask(in, "Username", a.AdminUser)
	for {
		pw := askSecret(in, "Password")
		if len([]rune(pw)) < setup.MinAdminPassword {
			fmt.Printf("  At least %d characters.\n", setup.MinAdminPassword)
			continue
		}
		again := askSecret(in, "Password again")
		if pw != again {
			fmt.Println("  They do not match.")
			continue
		}
		a.AdminPass = pw
		break
	}

	// --- the plan ---------------------------------------------------------
	if err := a.Validate(); err != nil {
		return fmt.Errorf("the answers are not usable:\n%w", err)
	}
	plan, err := setup.BuildPlan(a, paths, root)
	if err != nil {
		return err
	}

	fmt.Println()
	section("Plan")
	fmt.Print(plan.Describe(root))
	fmt.Println()

	if dryRun {
		fmt.Println("Dry run: nothing was written.")
		return nil
	}
	if len(plan.Existing) > 0 {
		fmt.Println("These already exist and will be REPLACED:")
		for _, p := range plan.Existing {
			fmt.Println("  " + p)
		}
		fmt.Println()
	}
	if !assumeYes && !askYesNo(in, "Write this", false) {
		fmt.Println("Nothing was written.")
		return nil
	}

	if err := plan.Apply(root, force); err != nil {
		return err
	}
	if err := plan.Provision(root); err != nil {
		return err
	}
	fmt.Println("Written.")

	// --- handing over -----------------------------------------------------
	fmt.Println()
	section("Next")
	printNextSteps(plan, root, a)
	return nil
}

func printNextSteps(plan *setup.Plan, root string, a setup.Answers) {
	paths := plan.Paths
	if root != "" && root != "/" {
		fmt.Printf("This was a test install under %s. Nothing on this machine runs it.\n", root)
		return
	}
	fmt.Println("Back up the master key somewhere other than this card:")
	fmt.Printf("    %s\n", paths.MasterKey())
	fmt.Println("It is what makes stored NTRIP passwords readable. A backup holding both it")
	fmt.Println("and the database is equivalent to a plaintext password list; keep them apart.")
	fmt.Println()
	fmt.Println("Then:")
	fmt.Printf("    sudo systemd-tmpfiles --create %s\n", paths.TmpfilesConf())
	fmt.Println("    sudo systemctl daemon-reload")
	if a.Archive.Enabled {
		unit := strings.TrimSuffix(filepathBase(paths.MountUnit(a.Archive.MountPoint)), "")
		fmt.Printf("    sudo systemctl enable --now %s\n", unit)
	}
	fmt.Println("    sudo systemctl enable --now psgnss.service")
	fmt.Println()
	fmt.Println("Check it came up:")
	fmt.Printf("    %s --check --config %s\n", paths.Binary(), paths.ConfigFile())
	fmt.Printf("    curl -s http://127.0.0.1%s/api/status\n", portOf(a.WebListen))
	fmt.Printf("    printf 'GET / HTTP/1.0\\r\\n\\r\\n' | nc 127.0.0.1 %s\n", strings.TrimPrefix(portOf(a.CasterListen), ":"))
	if len(plan.Conflicts) > 0 {
		fmt.Println()
		fmt.Println("These were found on this machine and will be stopped by systemd when")
		fmt.Println("psgnss.service starts, because only one process can hold the receiver:")
		for _, c := range plan.Conflicts {
			fmt.Println("    " + c)
		}
	}
	fmt.Println()
	fmt.Println("No NTRIP account exists yet. Add rovers in the web UI, or:")
	fmt.Printf("    %s --config %s --user-add name:password:5\n", paths.Binary(), paths.ConfigFile())
}

func filepathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ""
}

func section(name string) {
	fmt.Printf("\n── %s %s\n\n", name, strings.Repeat("─", max(0, 66-len(name))))
}

func ask(in *bufio.Reader, prompt, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", prompt, def)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		if errors.Is(err, io.EOF) {
			fmt.Println()
		}
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func askInt(in *bufio.Reader, prompt string, def int) int {
	for {
		s := ask(in, prompt, strconv.Itoa(def))
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err == nil {
			return n
		}
		fmt.Println("  A whole number, please.")
	}
}

func askFloat(in *bufio.Reader, prompt string, def float64) float64 {
	for {
		s := ask(in, prompt, strconv.FormatFloat(def, 'f', -1, 64))
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err == nil {
			return f
		}
		fmt.Println("  A number, please.")
	}
}

func askYesNo(in *bufio.Reader, prompt string, def bool) bool {
	d := "n"
	if def {
		d = "y"
	}
	for {
		switch strings.ToLower(ask(in, prompt+" (y/n)", d)) {
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Println("  y or n.")
	}
}

// askSecret reads without echo when stdin is a terminal. When it is not --- a
// scripted install, or a test --- it falls back to a normal read and says so,
// because silently echoing a password is worse than warning first.
//
// The echo is turned off through termios directly rather than by adding a
// terminal module: x/sys is already a dependency and the hub already speaks
// termios for the serial port.
func askSecret(in *bufio.Reader, prompt string) string {
	fd := int(os.Stdin.Fd())
	restore, ok := disableEcho(fd)
	if ok {
		defer restore()
		fmt.Printf("%s: ", prompt)
		line, _ := in.ReadString('\n')
		fmt.Println()
		return strings.TrimRight(line, "\r\n")
	}
	fmt.Printf("%s (not a terminal, this will be visible): ", prompt)
	line, _ := in.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

// disableEcho turns off terminal echo, returning a function that puts it back.
// The second result is false when stdin is not a terminal at all.
func disableEcho(fd int) (func(), bool) {
	original, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, false
	}
	quiet := *original
	quiet.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &quiet); err != nil {
		return nil, false
	}
	return func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, original) }, true
}
