package wkrsa

import (
	"testing"
)

// testPrivateKey is a freshly generated RSA-2048 key used ONLY for unit
// tests. It has no relationship to any production key material.
// Regenerated 2026-05-12 during pre-open-source secret scrub.
const testPrivateKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA0GTVoxu02WMdxeg+QxgbQ5C5rWQpDL3OLhzYUXOsmSdF2vUM
yJ5AFbv97Nt5DaNtmGzGldlQ9keNAvcljwGqR7F97jmxP+8kjqdOKqHYjj/Jt0FW
K/QlXAtogASPig3t26QGbfW8+qruoooxc2PKT5WulK9OfwgGvvdcLaHLlWZufIeE
cgdlQJ2qpNKOA/AgNTj16RspE9VdmKBIWzmGRDziFf1/pmmhBVe4TZ9MAfEUQIk7
Cm1eS/g8YwYsi2bI6MoK9Tcwh5IcEV8tvAXz+qSfuO31ep/5CvsSlcVvYYjBf3mQ
P7Dn811MDJFH5SzC4aknpQz2+c5GmHPJgYtTKQIDAQABAoIBAQDBoH8z70FpHwQB
59k6BAMJE0bCiabulMkm5VxEyiLbprbsS/YVzZwj1amI0x+2AVyKXL9jaikku7SU
xchbCKQLuyoUF/zON8gS1/bz+6839KLbJ9UGP/IahOsSz6oDDxArnUrwDn0Jt5rE
4XwzB8xph91Pf1eDBpUmCLXYHFYJuBau1pJT71aWQyK5IXemVk0zc8VYzjn5vYm7
FcaHVwMYVG/E5qnQKhcByYndAtyAoH6FrTXygtCRACbEMGR8wTz7dT4RNyLC72gQ
jHRxDIO+cSRK/+sDDKrQfr3njr2ZAhQ4wFbu7f0mEPO6k4TIydwMZMfiUEM8uz/o
Jfgwq4IRAoGBAOvLFXrZ1iVyaA1rUnfEn9omkGIDoD5yOdM+Altce7FTyrr3oViy
40lveMEfc+m/HsqSrUk1+UadDKAaB6vU84i9M7zTTc5v1odOBt0LTFSd4APUCoXG
GV9j4au5clYOQgLBfE2SOJ2YZgvHlatxG7YQg/Mtgv7EWirEHbJSM3F1AoGBAOJA
pxesAOQ7y2f5czxkU54sBA0go0SzMO9p4lC+w/nySyFGAtzRJOLia8mTgqZ5gQ4+
wA96dRjSyFBagOgjFLsofN5DhcV8UsxYeYTpV4IZDIVo9Y5S4OvsCAzAxKwC7uhv
VAwDzA+ZQeqp159gzhAMggSBmDK3nD0CvMdEP1BlAoGAa0Yhp5qjirXaEQDarBKQ
hzc0SONNbBubozd66wXQYIS2nwk6Jph8P1Svo20j1xxUbeT9YWlk13Nr4wr0ooBn
q7Yoa6fWpizLdRNSnA4f0/9fg15cyy+tK3DNosrj8bLa5VYRr1ju2QQUqRdMSItV
CCfLYD88cZvzSbGfsRkkvmECgYBLPv1TXh0dytUnS0sL9sHohPMD+qrSGlZYCXr/
J7K92dsqwcIJ9nSyEGOQssJs41QMjMoLW8q96rw8HR1qFuC6Lgj5UrOWrnZLB9HC
Zmh4GCSV6gZgwyeSzvkOZL4EByW1n/Dv3gNr3KiThtDzbJqbs805+m/HzlDj6Zkn
HIeCEQKBgGBSn1dd93zD52eBQ37NXsDRyCTKYY03Ux573Y/0Gb5TV2C6vJdnBtoW
zGtnCV4IyN9i7uoz/3F4q5b2vGvyQRC12c9ZILKdGfR3IqtMrXY9ns/2r5jhD9jz
1DwwVIoQy6qHRVApUesBCI7HfRqwTwY0ZtH1LqpZij8qSlc1X7Sr
-----END RSA PRIVATE KEY-----`

func TestSignWithMD5(t *testing.T) {
	sig, err := SignWithMD5([]byte("test"), []byte(testPrivateKey))
	if err != nil {
		t.Fatalf("SignWithMD5 failed: %v", err)
	}
	if sig == "" {
		t.Fatal("SignWithMD5 returned empty signature")
	}
}

func TestSignWithSHA256(t *testing.T) {
	sig, err := SignWithSHA256([]byte("test"), []byte(testPrivateKey))
	if err != nil {
		t.Fatalf("SignWithSHA256 failed: %v", err)
	}
	if sig == "" {
		t.Fatal("SignWithSHA256 returned empty signature")
	}
}

func TestSignWithSHA256_InvalidKey(t *testing.T) {
	_, err := SignWithSHA256([]byte("test"), []byte("invalid key"))
	if err == nil {
		t.Fatal("SignWithSHA256 should fail with invalid key")
	}
}
