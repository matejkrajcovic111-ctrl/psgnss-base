package unicore

import "testing"

func TestClassifyTable(t *testing.T) {
	cases := []struct {
		cmd   string
		class Class
		rev   bool
	}{
		{"VERSIONA", ClassQuery, false}, {"versionb", ClassQuery, false}, {"CONFIG", ClassQuery, false},
		{"config com1", ClassQuery, false}, {"MODE", ClassQuery, false}, {"MASK", ClassQuery, false},
		{"UNILOGLIST", ClassQuery, false}, {"BESTNAVA", ClassQuery, false}, {"SATSINFOA COM1", ClassQuery, false},
		{"BASEPOSA", ClassQuery, false},
		{"SAVECONFIG", ClassSave, false},
		{"CONFIG PPS DISABLE", ClassChange, true}, {"CONFIG COM2 115200", ClassChange, true},
		{"MODE BASE 1 TIME 60", ClassChange, true}, {"MODE BASE TIME 60 5", ClassChange, true},
		{"MODE BASE", ClassChange, true}, {"MODE BASE 3", ClassChange, true},
		{"MASK GPS", ClassChange, true}, {"MASK 10", ClassChange, true}, {"UNMASK GPS", ClassChange, true},
		{"RTCM1077 COM1 1", ClassChange, true}, {"GPGGA COM1 0.5", ClassChange, true},
		{"BESTNAVA ONCHANGED", ClassChange, true},
		{"UNLOG COM1 RTCM1077", ClassChange, true}, {"UNLOG GPGGA", ClassChange, true},
		{"FRESET", ClassDangerous, false}, {"freset all", ClassDangerous, false}, {"RESET", ClassDangerous, false},
		{"RESET EPHEM", ClassDangerous, false},
		{"UNLOG", ClassDangerous, true}, {"UNLOG COM1", ClassDangerous, true}, {"unlog com2", ClassDangerous, true},
		{"UNLOGALL", ClassDangerous, true},
		{"CONFIG COM1 460800", ClassDangerous, false}, {"config com1 115200 8 n 1", ClassDangerous, false},
		{"CONFIG SIGNALGROUP 2", ClassDangerous, false}, {"CONFIG CMDFORMAT 1", ClassDangerous, false},
		{"MODE ROVER", ClassDangerous, true}, {"MODE ROVER SURVEY", ClassDangerous, true},
		{"MODE BASE 48.1 17.1 300", ClassDangerous, true}, {"MODE BASE 1 48.1 17.1 300", ClassDangerous, true},
		{"MODE BASE -2160489.0276 4383620.1006 4084738.1110", ClassDangerous, true},
		{"UPGRADE", ClassRefused, false}, {"upgrade 115200", ClassRefused, false}, {"FWUPDATE", ClassRefused, false},
		{"loader", ClassRefused, false}, {"BOOTLOADER", ClassRefused, false}, {"DFU", ClassRefused, false},
		{"", ClassRefused, false}, {"  ", ClassRefused, false}, {"$command,x*00", ClassRefused, false},
		{"MODE\x00", ClassRefused, false}, {"CONFIG PPS é", ClassRefused, false},
	}
	for _, c := range cases {
		v := Classify(c.cmd)
		if v.Class != c.class {
			t.Errorf("Classify(%q) = %v, want %v", c.cmd, v.Class, c.class)
			continue
		}
		if (c.class == ClassChange || c.class == ClassDangerous) && v.Revertable != c.rev {
			t.Errorf("Classify(%q).Revertable = %v, want %v", c.cmd, v.Revertable, c.rev)
		}
		if (c.class == ClassDangerous || c.class == ClassRefused) && v.Reason == "" {
			t.Errorf("Classify(%q) gives no explanation", c.cmd)
		}
	}
}
