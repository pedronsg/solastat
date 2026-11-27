package setupmode

import (
	"strings"
	"unicode"
)

// APSSID is the setup network's name: "SolaStat_2ce8", where 2ce8 is what
// follows the dash in the device's name ("orbit-2ce8.local", as the address
// its wizard is reached at), so neighbouring devices don't share a name.
// Without such a name, the serial's last 4 letters/digits stand in.
func APSSID(host, serial string) string {
	tail := hostTail(host)
	if tail == "" {
		tail = serialTail(serial)
	}
	if tail == "" {
		return "SolaStat"
	}
	return "SolaStat_" + tail
}

// hostTail is what follows the last dash of host's first label, "" with none.
func hostTail(host string) string {
	label, _, _ := strings.Cut(host, ".")
	if i := strings.LastIndexByte(label, '-'); i >= 0 {
		return label[i+1:]
	}
	return ""
}

// serialTail is the serial's last 4 letters/digits.
func serialTail(serial string) string {
	var tail []rune
	for _, r := range strings.ToUpper(serial) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			tail = append(tail, r)
		}
	}
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return string(tail)
}
