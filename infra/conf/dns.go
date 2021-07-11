package conf

import (
	"encoding/json"

	"sort"
	"strings"

	"github.com/xtls/xray-core/app/dns"
	dm "github.com/xtls/xray-core/common/matcher/domain"
	"github.com/xtls/xray-core/common/matcher/domain/conf"
	"github.com/xtls/xray-core/common/matcher/geoip"
	"github.com/xtls/xray-core/common/matcher/geosite"
	"github.com/xtls/xray-core/common/net"
)

type NameServerConfig struct {
	Address      *Address
	ClientIP     *Address
	Port         uint16
	SkipFallback bool
	Domains      []string
	ExpectIPs    StringList
}

func (c *NameServerConfig) UnmarshalJSON(data []byte) error {
	var address Address
	if err := json.Unmarshal(data, &address); err == nil {
		c.Address = &address
		return nil
	}

	var advanced struct {
		Address      *Address   `json:"address"`
		ClientIP     *Address   `json:"clientIp"`
		Port         uint16     `json:"port"`
		SkipFallback bool       `json:"skipFallback"`
		Domains      []string   `json:"domains"`
		ExpectIPs    StringList `json:"expectIps"`
	}
	if err := json.Unmarshal(data, &advanced); err == nil {
		c.Address = advanced.Address
		c.ClientIP = advanced.ClientIP
		c.Port = advanced.Port
		c.SkipFallback = advanced.SkipFallback
		c.Domains = advanced.Domains
		c.ExpectIPs = advanced.ExpectIPs
		return nil
	}

	return newError("failed to parse name server: ", string(data))
}

func (c *NameServerConfig) Build() (*dns.NameServer, error) {
	if c.Address == nil {
		return nil, newError("NameServer address is not specified.")
	}

	var domains []*dm.Domain
	var originalRules []*dns.NameServer_OriginalRule

	for _, rule := range c.Domains {
		parsedDomain, err := conf.ParseDomainRule(rule)
		if err != nil {
			return nil, newError("invalid domain rule: ", rule).Base(err)
		}

		for _, pd := range parsedDomain {
			domains = append(domains, &dm.Domain{
				Type:  pd.Type,
				Value: pd.Value,
			})
		}
		originalRules = append(originalRules, &dns.NameServer_OriginalRule{
			Rule: rule,
			Size: uint32(len(parsedDomain)),
		})
	}

	geoipList, err := geoip.ParseIPList(c.ExpectIPs)
	if err != nil {
		return nil, newError("invalid IP rule: ", c.ExpectIPs).Base(err)
	}

	var myClientIP []byte
	if c.ClientIP != nil {
		if !c.ClientIP.Family().IsIP() {
			return nil, newError("not an IP address:", c.ClientIP.String())
		}
		myClientIP = []byte(c.ClientIP.IP())
	}
	return &dns.NameServer{
		Address: &net.Endpoint{
			Network: net.Network_UDP,
			Address: c.Address.Build(),
			Port:    uint32(c.Port),
		},
		ClientIp:          myClientIP,
		SkipFallback:      c.SkipFallback,
		PrioritizedDomain: domains,
		Geoip:             geoipList,
		OriginalRules:     originalRules,
	}, nil
}

// DNSConfig is a JSON serializable object for dns.Config.
type DNSConfig struct {
	Servers         []*NameServerConfig     `json:"servers"`
	Hosts           map[string]*HostAddress `json:"hosts"`
	ClientIP        *Address                `json:"clientIp"`
	Tag             string                  `json:"tag"`
	QueryStrategy   string                  `json:"queryStrategy"`
	CacheStrategy   string                  `json:"cacheStrategy"`
	DisableCache    bool                    `json:"disableCache"`
	DisableFallback bool                    `json:"disableFallback"`
}

type HostAddress struct {
	addr  *Address
	addrs []*Address
}

// UnmarshalJSON implements encoding/json.Unmarshaler.UnmarshalJSON
func (h *HostAddress) UnmarshalJSON(data []byte) error {
	addr := new(Address)
	var addrs []*Address
	switch {
	case json.Unmarshal(data, &addr) == nil:
		h.addr = addr
	case json.Unmarshal(data, &addrs) == nil:
		h.addrs = addrs
	default:
		return newError("invalid address")
	}
	return nil
}

func getHostMapping(ha *HostAddress) *dns.Config_HostMapping {
	if ha.addr != nil {
		if ha.addr.Family().IsDomain() {
			return &dns.Config_HostMapping{
				ProxiedDomain: ha.addr.Domain(),
			}
		}
		return &dns.Config_HostMapping{
			Ip: [][]byte{ha.addr.IP()},
		}
	}

	ips := make([][]byte, 0, len(ha.addrs))
	for _, addr := range ha.addrs {
		if addr.Family().IsDomain() {
			return &dns.Config_HostMapping{
				ProxiedDomain: addr.Domain(),
			}
		}
		ips = append(ips, []byte(addr.IP()))
	}
	return &dns.Config_HostMapping{
		Ip: ips,
	}
}

// Build implements Buildable
func (c *DNSConfig) Build() (*dns.Config, error) {
	config := &dns.Config{
		Tag:             c.Tag,
		CacheStrategy:   dns.CacheStrategy_Cache_ALL,
		DisableFallback: c.DisableFallback,
	}

	if c.DisableCache {
		config.CacheStrategy = dns.CacheStrategy_Cache_DISABLE
	}

	if c.ClientIP != nil {
		if !c.ClientIP.Family().IsIP() {
			return nil, newError("not an IP address:", c.ClientIP.String())
		}
		config.ClientIp = []byte(c.ClientIP.IP())
	}

	config.QueryStrategy = dns.QueryStrategy_USE_IP
	switch strings.ToLower(c.QueryStrategy) {
	case "useip", "use_ip", "use-ip":
		config.QueryStrategy = dns.QueryStrategy_USE_IP
	case "useip4", "useipv4", "use_ip4", "use_ipv4", "use_ip_v4", "use-ip4", "use-ipv4", "use-ip-v4":
		config.QueryStrategy = dns.QueryStrategy_USE_IP4
	case "useip6", "useipv6", "use_ip6", "use_ipv6", "use_ip_v6", "use-ip6", "use-ipv6", "use-ip-v6":
		config.QueryStrategy = dns.QueryStrategy_USE_IP6
	}

	switch strings.ToLower(c.CacheStrategy) {
	case "noerror":
		config.CacheStrategy = dns.CacheStrategy_Cache_NOERROR
	case "all":
		config.CacheStrategy = dns.CacheStrategy_Cache_ALL
	case "disable", "none":
		config.CacheStrategy = dns.CacheStrategy_Cache_DISABLE
	}

	for _, server := range c.Servers {
		ns, err := server.Build()
		if err != nil {
			return nil, newError("failed to build nameserver").Base(err)
		}
		config.NameServer = append(config.NameServer, ns)
	}

	if c.Hosts != nil && len(c.Hosts) > 0 {
		domains := make([]string, 0, len(c.Hosts))
		for domain := range c.Hosts {
			domains = append(domains, domain)
		}
		sort.Strings(domains)

		for _, domain := range domains {
			addr := c.Hosts[domain]
			var mappings []*dns.Config_HostMapping
			switch {
			case strings.HasPrefix(domain, "domain:"):
				domainName := domain[7:]
				if len(domainName) == 0 {
					return nil, newError("empty domain type of rule: ", domain)
				}
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Subdomain
				mapping.Domain = domainName
				mappings = append(mappings, mapping)

			case strings.HasPrefix(domain, "geosite:"):
				listName := domain[8:]
				if len(listName) == 0 {
					return nil, newError("empty geosite rule: ", domain)
				}
				domains, err := geosite.LoadGeositeWithAttr("geosite.dat", listName)
				if err != nil {
					return nil, newError("failed to load geosite: ", listName).Base(err)
				}
				for _, d := range domains {
					mapping := getHostMapping(addr)
					mapping.Type = d.Type
					mapping.Domain = d.Value
					mappings = append(mappings, mapping)
				}

			case strings.HasPrefix(domain, "regexp:"):
				regexpVal := domain[7:]
				if len(regexpVal) == 0 {
					return nil, newError("empty regexp type of rule: ", domain)
				}
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Regex
				mapping.Domain = regexpVal
				mappings = append(mappings, mapping)

			case strings.HasPrefix(domain, "keyword:"):
				keywordVal := domain[8:]
				if len(keywordVal) == 0 {
					return nil, newError("empty keyword type of rule: ", domain)
				}
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Keyword
				mapping.Domain = keywordVal
				mappings = append(mappings, mapping)

			case strings.HasPrefix(domain, "full:"):
				fullVal := domain[5:]
				if len(fullVal) == 0 {
					return nil, newError("empty full domain type of rule: ", domain)
				}
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Full
				mapping.Domain = fullVal
				mappings = append(mappings, mapping)

			case strings.HasPrefix(domain, "dotless:"):
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Regex
				switch substr := domain[8:]; {
				case substr == "":
					mapping.Domain = "^[^.]*$"
				case !strings.Contains(substr, "."):
					mapping.Domain = "^[^.]*" + substr + "[^.]*$"
				default:
					return nil, newError("substr in dotless rule should not contain a dot: ", substr)
				}
				mappings = append(mappings, mapping)

			case strings.HasPrefix(domain, "ext:"):
				kv := strings.Split(domain[4:], ":")
				if len(kv) != 2 {
					return nil, newError("invalid external resource: ", domain)
				}
				filename := kv[0]
				list := kv[1]
				domains, err := geosite.LoadGeositeWithAttr(filename, list)
				if err != nil {
					return nil, newError("failed to load domain list: ", list, " from ", filename).Base(err)
				}
				for _, d := range domains {
					mapping := getHostMapping(addr)
					mapping.Type = d.Type
					mapping.Domain = d.Value
					mappings = append(mappings, mapping)
				}

			default:
				mapping := getHostMapping(addr)
				mapping.Type = dm.MatchingType_Full
				mapping.Domain = domain
				mappings = append(mappings, mapping)
			}

			config.StaticHosts = append(config.StaticHosts, mappings...)
		}
	}

	return config, nil
}
