package probe

import (
	"reflect"
	"testing"
)

func TestFindSSLLibs(t *testing.T) {
	maps := `55d0c0a00000-55d0c0a2c000 r--p 00000000 00:2f 1234 /usr/bin/python3.11
7f1a2b000000-7f1a2b01f000 r--p 00000000 00:2f 2001 /usr/lib/x86_64-linux-gnu/libssl.so.3
7f1a2b01f000-7f1a2b07a000 r-xp 0001f000 00:2f 2001 /usr/lib/x86_64-linux-gnu/libssl.so.3
7f1a2c000000-7f1a2c200000 r-xp 00000000 00:2f 2002 /usr/lib/x86_64-linux-gnu/libcrypto.so.3
7f1a2d000000-7f1a2d040000 r-xp 00000000 00:2f 2003 /opt/my app/lib/libssl.so.1.1
7f1a2e000000-7f1a2e040000 r-xp 00000000 00:2f 2004 /usr/lib/x86_64-linux-gnu/libssl3.so
7f1a2f000000-7f1a2f040000 r-xp 00000000 00:2f 2005 /tmp/old/libssl.so.3 (deleted)
7f1a30000000-7f1a30040000 r-xp 00000000 00:2f 2006 /usr/lib/libssl.so
7ffd1c000000-7ffd1c021000 rw-p 00000000 00:00 0 [stack]
7ffd1d000000-7ffd1d002000 r-xp 00000000 00:00 0 [vdso]
7f1a31000000-7f1a31001000 rw-p 00000000 00:00 0
`
	got := findSSLLibs([]byte(maps))
	want := []string{
		"/usr/lib/x86_64-linux-gnu/libssl.so.3",
		"/opt/my app/lib/libssl.so.1.1",
		"/usr/lib/libssl.so",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findSSLLibs = %q, want %q", got, want)
	}
	if got := findSSLLibs(nil); len(got) != 0 {
		t.Fatalf("empty maps: %q", got)
	}
}

func TestProcStartTime(t *testing.T) {
	stat := "4242 (my (odd) proc) S 1 4242 4242 0 -1 4194560 120 0 0 0 1 2 0 0 20 0 1 0 987654 12345678 300 18446744073709551615"
	if got := procStartTime([]byte(stat)); got != "987654" {
		t.Fatalf("start time %q, want 987654", got)
	}
	if got := procStartTime([]byte("12 (x) S 1 2")); got != "" {
		t.Fatalf("short stat: %q", got)
	}
	if got := procStartTime([]byte("garbage")); got != "" {
		t.Fatalf("garbage: %q", got)
	}
}
