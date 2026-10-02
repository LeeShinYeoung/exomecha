package template

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

const hostsPath = ".exomecha/allowed-hosts"

func (e *Env) AllowedHosts(ctx context.Context) ([]string, error) {
	content, ok, err := e.files.BaseFile(ctx, hostsPath)
	if err != nil || !ok {
		return nil, err
	}
	return parseHosts(content)
}

// One pattern per line in sbx policy syntax. A pattern sbx would reject should stop here: sbx
// refusing it for every new sandbox looks like a temporary sbx failure, which is retried forever.
func parseHosts(content string) ([]string, error) {
	var hosts []string
	for i, line := range strings.Split(content, "\n") {
		line, _, _ = strings.Cut(line, "#")
		host := strings.TrimSpace(line)
		if host == "" {
			continue
		}
		if why := badHost(host); why != "" {
			return nil, &Problem{Reason: fmt.Sprintf("`%s` line %d (`%s`) %s Fix it on the base branch.", hostsPath, i+1, host, why)}
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

func badHost(host string) string {
	switch {
	case strings.IndexFunc(host, unicode.IsSpace) >= 0, strings.Contains(host, ","):
		return "holds more than one pattern. Write one host pattern per line."
	case host == "**" || strings.HasPrefix(host, "**:"):
		return "allows every host. List only the hosts the project needs."
	case strings.Contains(host, "://"):
		return "is a URL. Write only the host, such as api.example.com."
	case strings.IndexFunc(host, notHostChar) >= 0:
		return "has a character sbx does not accept in a host pattern."
	}
	return ""
}

// Letters, digits, "-", "_" and the globs, as sbx lists them, plus the dots, the port colon, the
// brackets of an IPv6 address, and the slash of a CIDR prefix.
func notHostChar(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("-_.*?[]!:/", r)
}
