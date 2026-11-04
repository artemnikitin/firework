package network

import (
	"reflect"
	"strings"
	"testing"
)

// Listed by iptables 1.8.11 (nf_tables) for rules the agent appended: the
// listing reorders matches (-d before -i) and quotes the comment.
const listedChain = `-P PREROUTING ACCEPT
-A PREROUTING -d 10.0.0.5/32 -i eth0 -p tcp -m tcp --dport 8000 -m comment --comment "firework:pf:app" -j DNAT --to-destination 172.16.0.2:80
-A PREROUTING -d 10.0.0.5/32 -i br0 -p tcp -m tcp --dport 8000 -m comment --comment "firework:pf:app" -j DNAT --to-destination 172.16.0.2:80
-A PREROUTING -p tcp -m tcp --dport 9000 -j DNAT --to-destination 172.16.0.3:90
-A PREROUTING -p udp -m udp --dport 5353 -j DNAT --to-destination 172.16.0.4:53
-A PREROUTING -m addrtype --dst-type LOCAL -j DOCKER
-A PREROUTING -p tcp -m tcp --dport 7000:7010 -j DNAT --to-destination 172.16.0.5
-A PREROUTING -p tcp -m comment --comment "say \"hi\" \\ there" -m tcp --dport 7100 -j DNAT --to-destination 172.16.0.6`

func TestParsePortForwardListing(t *testing.T) {
	t.Parallel()

	listing, err := ParsePortForwardListing(listedChain)
	if err != nil {
		t.Fatal(err)
	}
	want := []PortForwardRule{
		{Interface: "eth0", HostIP: "10.0.0.5", HostPort: 8000, GuestIP: "172.16.0.2", VMPort: 80, Tag: "firework:pf:app"},
		{Interface: "br0", HostIP: "10.0.0.5", HostPort: 8000, GuestIP: "172.16.0.2", VMPort: 80, Tag: "firework:pf:app"},
		{HostPort: 9000, GuestIP: "172.16.0.3", VMPort: 90},
		{HostPort: 7100, GuestIP: "172.16.0.6", VMPort: 7100, Tag: `say "hi" \ there`},
	}
	if len(listing.Rules) != len(want) {
		t.Fatalf("rules = %#v", listing.Rules)
	}
	for i, rule := range listing.Rules {
		rule.Args = nil
		if !reflect.DeepEqual(rule, want[i]) {
			t.Errorf("rule %d = %#v, want %#v", i, rule, want[i])
		}
	}
	// A port range may cover any claimed port, so it is reported, not dropped.
	if len(listing.Unparsed) != 1 || !strings.Contains(listing.Unparsed[0], "7000:7010") {
		t.Fatalf("unparsed = %#v", listing.Unparsed)
	}
	if !listing.Rules[0].TaggedFor("app") || listing.Rules[2].Tagged() || listing.Rules[3].Tagged() {
		t.Fatalf("tags misread: %#v", listing.Rules)
	}
}

// Deleting by the listed arguments must name exactly the rule that was
// listed, including its unquoted comment.
func TestListedRuleArgsDeleteTheSameRule(t *testing.T) {
	t.Parallel()

	listing, err := ParsePortForwardListing(listedChain)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(listing.Rules[0].Args, "|")
	want := "-d|10.0.0.5/32|-i|eth0|-p|tcp|-m|tcp|--dport|8000|-m|comment|--comment|firework:pf:app|-j|DNAT|--to-destination|172.16.0.2:80"
	if got != want {
		t.Fatalf("args = %s", got)
	}
}

func TestInstalledSpecParsesBackToItsTuple(t *testing.T) {
	t.Parallel()

	spec := tagged(scopedPortForwardSpec("enp3s0", "10.0.100.91", 19300, "172.16.0.6", 9300), PortForwardTag("es"))
	listing, err := ParsePortForwardListing("-A PREROUTING " + strings.Join(spec, " "))
	if err != nil || len(listing.Rules) != 1 {
		t.Fatalf("listing = %#v err = %v", listing, err)
	}
	rule := listing.Rules[0]
	if !rule.Targets(19300, "172.16.0.6", 9300) || !rule.TaggedFor("es") || rule.HostIP != "10.0.100.91" || rule.Interface != "enp3s0" {
		t.Fatalf("rule = %#v", rule)
	}
}

func TestPortForwardTag(t *testing.T) {
	t.Parallel()

	if got := PortForwardTag("tenant-a.web_1"); got != "firework:pf:tenant-a.web_1" {
		t.Fatalf("plain name tagged as %q", got)
	}
	long := PortForwardTag(strings.Repeat("x", 129))
	odd := PortForwardTag(`a "quoted" name`)
	for _, tag := range []string{long, odd} {
		if !strings.HasPrefix(tag, "firework:pf:h-") || len(tag) > 64 || strings.ContainsAny(tag, "\" ") {
			t.Fatalf("hashed tag %q", tag)
		}
	}
	if long == odd {
		t.Fatal("distinct names share a tag")
	}
}
