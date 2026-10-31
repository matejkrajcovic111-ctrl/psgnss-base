package boardprofile

import "testing"

func TestResolveX20PCompatibility(t *testing.T) {
	p, err := Resolve("", "ZED-X20P")
	if err != nil || p.ID != "simplertk4-optimum" || !p.Implemented || p.Driver != DriverUBXVal {
		t.Fatalf("Resolve X20P = %+v, %v", p, err)
	}
}

func TestResolveUM980(t *testing.T) {
	p, err := Resolve("", "UM980")
	if err != nil || p.ID != "simplertk3b-budget" || !p.Unicore() || p.UBX() || !p.NativeEphemeris {
		t.Fatalf("Resolve UM980 = %+v, %v", p, err)
	}
	if x, _ := Resolve("", "ZED-X20P"); !x.UBX() || x.Unicore() || x.NativeEphemeris {
		t.Fatalf("the X20P profile changed: %+v", x)
	}
}

func TestStubProfilesAreExplicit(t *testing.T) {
	for _, id := range []string{"simplertk2b", "simplertk3b-pro"} {
		p, err := Resolve(id, "")
		if err != nil || p.Implemented || p.Driver != DriverStub || len(p.FormFactors) == 0 {
			t.Fatalf("stub %s = %+v, %v", id, p, err)
		}
	}
}

func TestResolveRejectsMismatch(t *testing.T) {
	if _, err := Resolve("simplertk3b-pro", "ZED-X20P"); err == nil {
		t.Fatal("accepted a profile/model mismatch")
	}
}
