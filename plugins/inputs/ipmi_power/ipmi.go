package ipmi_power

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/inputs"
)

var (
	execCommand             = exec.Command // execCommand is used to mock commands in tests.
	re_parse_line        = regexp.MustCompile(`^\s+(?P<name>[^:]*):\s+(?P<value>\S+)\s+(?P<unit>\S+)`)
)

// Ipmi stores the configuration values for the ipmi_power input plugin
type Ipmi struct {
	Path          string
	Privilege     string
	Servers       []string
	Timeout       internal.Duration
	UseSudo       bool
	SamplePeriod  string
}

var sampleConfig = `
  ## optionally specify the path to the ipmitool executable
  # path = "/usr/bin/ipmitool"
  ##
  ## Setting 'use_sudo' to true will make use of sudo to run ipmitool.
  ## Sudo must be configured to allow the telegraf user to run ipmitool
  ## without a password.
  # use_sudo = false
  ##
  ## optionally force session privilege level. Can be CALLBACK, USER, OPERATOR, ADMINISTRATOR
  # privilege = "ADMINISTRATOR"
  ##
  ## optionally specify one or more servers via a url matching
  ##  [username[:password]@][protocol[(address)]]
  ##  e.g.
  ##    root:passwd@lan(127.0.0.1)
  ##
  ## if no servers are specified, local machine sensor stats will be queried
  ##
  # servers = ["USERID:PASSW0RD@lan(192.168.1.1)"]

  ## Recommended: use metric 'interval' that is a multiple of 'timeout' to avoid
  ## gaps or overlap in pulled data
  interval = "30s"

  ## Timeout for the ipmitool command to complete
  timeout = "20s"

  ## Sample Period, can be 5_sec/15_sec/30_sec/1_min/3_min/7_min/15_min/30_min/1_hour
  # sample_period = ""
`

// SampleConfig returns the documentation about the sample configuration
func (m *Ipmi) SampleConfig() string {
	return sampleConfig
}

// Description returns a basic description for the plugin functions
func (m *Ipmi) Description() string {
	return "Read metrics from the bare metal servers via IPMI"
}

// Gather is the main execution function for the plugin
func (m *Ipmi) Gather(acc telegraf.Accumulator) error {
	if len(m.Path) == 0 {
		return fmt.Errorf("ipmitool not found: verify that ipmitool is installed and that ipmitool is in your PATH")
	}

	if len(m.Servers) > 0 {
		wg := sync.WaitGroup{}
		for _, server := range m.Servers {
			wg.Add(1)
			go func(a telegraf.Accumulator, s string) {
				defer wg.Done()
				err := m.parse(a, s)
				if err != nil {
					a.AddError(err)
				}
			}(acc, server)
		}
		wg.Wait()
	} else {
		err := m.parse(acc, "")
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *Ipmi) parse(acc telegraf.Accumulator, server string) error {
	opts := make([]string, 0)
	hostname := ""
	if server != "" {
		conn := NewConnection(server, m.Privilege)
		hostname = conn.Hostname
		opts = conn.options()
	}
	opts = append(opts, "dcmi", "power", "reading")

	if m.SamplePeriod != "" {
		opts = append(opts, m.SamplePeriod)
	}

	name := m.Path
	if m.UseSudo {
		// -n - avoid prompting the user for input of any kind
		opts = append([]string{"-n", name}, opts...)
		name = "sudo"
	}
	cmd := execCommand(name, opts...)
	out, err := internal.CombinedOutputTimeout(cmd, m.Timeout.Duration)
	timestamp := time.Now()
	if err != nil {
		return fmt.Errorf("failed to run command %s: %s - %s", strings.Join(cmd.Args, " "), err, string(out))
	}
	return parseInner(acc, hostname, out, timestamp)
}

func parseInner(acc telegraf.Accumulator, hostname string, cmdOut []byte, measured_at time.Time) error {
	// each line will look something like
	// Planar VBAT      | 3.05 Volts        | ok

	fields := make(map[string]interface{})
	scanner := bufio.NewScanner(bytes.NewReader(cmdOut))
	for scanner.Scan() {
		ipmiFields := extractFieldsFromRegex(re_parse_line, scanner.Text())
		if len(ipmiFields) != 3 {
			continue
		}

		key := transform(ipmiFields["name"])
		floatval, err := aToFloat(ipmiFields["value"])
		if err != nil {
			continue
		}
		fields[key] = floatval
		fields[key + "_unit"] = ipmiFields["unit"]

	}

	acc.AddFields("ipmi_power", fields, nil, measured_at)

	return scanner.Err()
}

// extractFieldsFromRegex consumes a regex with named capture groups and returns a kvp map of strings with the results
func extractFieldsFromRegex(re *regexp.Regexp, input string) map[string]string {
	submatches := re.FindStringSubmatch(input)
	results := make(map[string]string)
	subexpNames := re.SubexpNames()
	if len(subexpNames) > len(submatches) {
		log.Printf("D! No matches found in '%s'", input)
		return results
	}
	for i, name := range subexpNames {
		if name != input && name != "" && input != "" {
			results[name] = trim(submatches[i])
		}
	}
	return results
}

// aToFloat converts string representations of numbers to float64 values
func aToFloat(val string) (float64, error) {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0.0, err
	}
	return f, nil
}

func trim(s string) string {
	return strings.TrimSpace(s)
}

func transform(s string) string {
	s = trim(s)
	s = strings.ToLower(s)
	return strings.Replace(s, " ", "_", -1)
}

func init() {
	m := Ipmi{}
	path, _ := exec.LookPath("ipmitool")
	if len(path) > 0 {
		m.Path = path
	}
	m.Timeout = internal.Duration{Duration: time.Second * 20}
	inputs.Add("ipmi_power", func() telegraf.Input {
		m := m
		return &m
	})
}
