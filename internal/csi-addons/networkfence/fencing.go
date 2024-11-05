/*
Copyright 2023 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package networkfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ceph/ceph-csi/internal/util"
	"github.com/ceph/ceph-csi/internal/util/log"

	"github.com/csi-addons/spec/lib/go/fence"
)

const (
	blocklistTime     = "157784760"
	invalidCommandStr = "invalid command"
	// we can always use mds rank 0, since all the clients have a session with rank-0.
	mdsRank = 0
)

// NetworkFence contains the CIDR blocks to be blocked.
type NetworkFence struct {
	Cidr     []string
	Monitors string
	cr       *util.Credentials
}

// activeClient represents the structure of an active client.
type activeClient struct {
	Inst string `json:"inst"`
}

// IPWithNonce represents the structure of an IP with nonce
// as listed by Ceph OSD blocklist.
type IPWithNonce struct {
	IP    string `json:"ip"`
	Nonce string `json:"nonce"`
}

// NewNetworkFence returns a networkFence struct object from the Network fence/unfence request.
func NewNetworkFence(
	ctx context.Context,
	cr *util.Credentials,
	cidrs []*fence.CIDR,
	fenceOptions map[string]string,
) (*NetworkFence, error) {
	var err error
	nwFence := &NetworkFence{}

	nwFence.Cidr, err = GetCIDR(cidrs)
	if err != nil {
		return nil, fmt.Errorf("failed to get list of CIDRs: %w", err)
	}

	clusterID, err := util.GetClusterID(fenceOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch clusterID: %w", err)
	}

	nwFence.Monitors, _, err = util.GetMonsAndClusterID(ctx, clusterID, false)
	if err != nil {
		return nil, fmt.Errorf("failed to get monitors for clusterID %q: %w", clusterID, err)
	}

	nwFence.cr = cr

	return nwFence, nil
}

// addCephBlocklist adds an IP to ceph osd blocklist.
func (nf *NetworkFence) addCephBlocklist(ctx context.Context, ip string, useRange bool) error {
	arg := []string{
		"--id", nf.cr.ID,
		"--keyfile=" + nf.cr.KeyFile,
		"-m", nf.Monitors,
	}
	// TODO: add blocklist till infinity.
	// Currently, ceph does not provide the functionality to blocklist IPs
	// for infinite time. As a workaround, add a blocklist for 5 YEARS to
	// represent infinity from ceph-csi side.
	// At any point in this time, the IPs can be unblocked by an UnfenceClusterReq.
	// This needs to be updated once ceph provides functionality for the same.
	cmd := []string{"osd", "blocklist"}
	if useRange {
		cmd = append(cmd, "range")
	}
	cmd = append(cmd, "add", ip, blocklistTime)
	cmd = append(cmd, arg...)
	_, stdErr, err := util.ExecCommand(ctx, "ceph", cmd...)
	if err != nil {
		return fmt.Errorf("failed to blocklist IP %q: %w stderr: %q", ip, err, stdErr)
	}
	log.DebugLog(ctx, "blocklisted IP %q successfully", ip)

	return nil
}

// AddNetworkFence blocks access for all the IPs in the IP range mentioned via the CIDR block
// using a network fence.
func (nf *NetworkFence) AddNetworkFence(ctx context.Context) error {
	hasBlocklistRangeSupport := true
	// for each CIDR block, convert it into a range of IPs so as to perform blocklisting operation.
	for _, cidr := range nf.Cidr {
		// try range blocklist cmd, if invalid fallback to
		// iterating through IP range.
		if hasBlocklistRangeSupport {
			err := nf.addCephBlocklist(ctx, cidr, true)
			if err == nil {
				continue
			}
			if !strings.Contains(err.Error(), invalidCommandStr) {
				return fmt.Errorf("failed to add blocklist range %q: %w", cidr, err)
			}
			hasBlocklistRangeSupport = false
		}
		// fetch the list of IPs from a CIDR block
		hosts, err := getIPRange(cidr)
		if err != nil {
			return fmt.Errorf("failed to convert CIDR block %s to corresponding IP range: %w", cidr, err)
		}

		// add ceph blocklist for each IP in the range mentioned by the CIDR
		for _, host := range hosts {
			err = nf.addCephBlocklist(ctx, host, false)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (nf *NetworkFence) listActiveClients(ctx context.Context) ([]activeClient, error) {
	arg := []string{
		"--id", nf.cr.ID,
		"--keyfile=" + nf.cr.KeyFile,
		"-m", nf.Monitors,
	}
	// FIXME: replace the ceph command with go-ceph API in future
	cmd := []string{"tell", fmt.Sprintf("mds.%d", mdsRank), "client", "ls"}
	cmd = append(cmd, arg...)
	stdout, stdErr, err := util.ExecCommandWithTimeout(ctx, 2*time.Minute, "ceph", cmd...)
	if err != nil {
		return nil, fmt.Errorf("failed to list active clients: %w, stderr: %q", err, stdErr)
	}

	var activeClients []activeClient
	if err := json.Unmarshal([]byte(stdout), &activeClients); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return activeClients, nil
}

func (nf *NetworkFence) evictCephFSClient(ctx context.Context, clientID int) error {
	arg := []string{
		"--id", nf.cr.ID,
		"--keyfile=" + nf.cr.KeyFile,
		"-m", nf.Monitors,
	}
	// FIXME: replace the ceph command with go-ceph API in future
	cmd := []string{"tell", fmt.Sprintf("mds.%d", mdsRank), "client", "evict", fmt.Sprintf("id=%d", clientID)}
	cmd = append(cmd, arg...)
	_, stdErr, err := util.ExecCommandWithTimeout(ctx, 2*time.Minute, "ceph", cmd...)
	if err != nil {
		return fmt.Errorf("failed to evict client %d: %w, stderr: %q", clientID, err, stdErr)
	}
	log.DebugLog(ctx, "client %s has been evicted from CephFS\n", clientID)

	return nil
}

func isIPInCIDR(ctx context.Context, ip, cidr string) bool {
	// Parse the CIDR block
	_, ipCidr, err := net.ParseCIDR(cidr)
	if err != nil {
		log.ErrorLog(ctx, "error parsing CIDR block %s: %w\n", cidr, err)

		return false
	}

	// Parse the IP address
	ipAddress := net.ParseIP(ip)
	if ipAddress == nil {
		log.ErrorLog(ctx, "error parsing IP address %s\n", ip)

		return false
	}

	// Check if the IP address is within the CIDR block
	return ipCidr.Contains(ipAddress)
}

func (ac *activeClient) fetchIP() (string, error) {
	// example: "inst": "client.4305 172.21.9.34:0/422650892",
	// then returning value will be 172.21.9.34
	return ParseClientIP(ac.Inst)
}

func (ac *activeClient) fetchID() (int, error) {
	// example: "inst": "client.4305 172.21.9.34:0/422650892",
	// then returning value will be 4305
	clientInfo := ac.Inst
	parts := strings.Fields(clientInfo)
	if len(parts) >= 1 {
		clientIDStr := strings.TrimPrefix(parts[0], "client.")
		clientID, err := strconv.Atoi(clientIDStr)
		if err != nil {
			return 0, fmt.Errorf("failed to convert client ID to int: %w", err)
		}

		return clientID, nil
	}

	return 0, fmt.Errorf("failed to extract client ID, incorrect format: %s", clientInfo)
}

// AddClientEviction blocks access for all the IPs in the CIDR block
// using client eviction, it also blocks the entire CIDR.
func (nf *NetworkFence) AddClientEviction(ctx context.Context) error {
	evictedIPs := make(map[string]bool)
	// fetch active clients
	activeClients, err := nf.listActiveClients(ctx)
	if err != nil {
		return err
	}
	// iterate through CIDR blocks and check if any active client matches
	for _, cidr := range nf.Cidr {
		for _, client := range activeClients {
			var clientIP string
			clientIP, err = client.fetchIP()
			if err != nil {
				return fmt.Errorf("error fetching client IP: %w", err)
			}
			// check if the clientIP is in the CIDR block
			if isIPInCIDR(ctx, clientIP, cidr) {
				var clientID int
				clientID, err = client.fetchID()
				if err != nil {
					return fmt.Errorf("error fetching client ID: %w", err)
				}
				// evict the client
				err = nf.evictCephFSClient(ctx, clientID)
				if err != nil {
					return fmt.Errorf("error evicting client %d: %w", clientID, err)
				}
				log.DebugLog(ctx, "client %d has been evicted\n", clientID)
				// add the CIDR to the list of blocklisted IPs
				evictedIPs[clientIP] = true
			}
		}
	}

	// add the range based blocklist for CIDR
	err = nf.AddNetworkFence(ctx)
	if err != nil {
		return err
	}

	return nil
}

// getIPRange returns a list of IPs from the IP range
// corresponding to a CIDR block.
func getIPRange(cidr string) ([]string, error) {
	var hosts []string
	netIP, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}
	for ip := netIP.Mask(ipnet.Mask); ipnet.Contains(ip); incIP(ip) {
		hosts = append(hosts, ip.String())
	}

	return hosts, nil
}

// incIP is an helper function for getIPRange() for incrementing
// IP values to return all IPs in a range.
func incIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// Cidrs is a list of CIDR structs.
type Cidrs []*fence.CIDR

// GetCIDR converts a CIDR struct list to a list.
func GetCIDR(cidrs Cidrs) ([]string, error) {
	var cidrList []string
	for _, cidr := range cidrs {
		cidrList = append(cidrList, cidr.GetCidr())
	}
	if len(cidrList) < 1 {
		return nil, errors.New("the CIDR cannot be empty")
	}

	return cidrList, nil
}

// removeCephBlocklist removes an IP from ceph osd blocklist.
// the value of nonce is ignored if useRange is true.
func (nf *NetworkFence) removeCephBlocklist(ctx context.Context, ip, nonce string, useRange bool) error {
	arg := []string{
		"--id", nf.cr.ID,
		"--keyfile=" + nf.cr.KeyFile,
		"-m", nf.Monitors,
	}
	cmd := []string{"osd", "blocklist"}
	if useRange {
		cmd = append(cmd, "range")
	}

	// If nonce is not empty and we are not using
	// range based blocks, we need to add the nonce
	if nonce != "" && !useRange {
		cmd = append(cmd, "rm", fmt.Sprintf("%s:0/%s", ip, nonce))
	} else {
		cmd = append(cmd, "rm", ip)
	}

	cmd = append(cmd, arg...)

	_, stdErr, err := util.ExecCommand(ctx, "ceph", cmd...)
	if err != nil {
		return fmt.Errorf("failed to unblock IP %q: %v %w", ip, stdErr, err)
	}
	log.DebugLog(ctx, "unblocked IP %q successfully", ip)

	return nil
}

// RemoveNetworkFence unblocks access for all the IPs in the IP range mentioned via the CIDR block
// using a network fence.
// Unfencing one of the protocols(CephFS or RBD) suggests the node is expected to be recovered, so
// both CephFS and RBD are expected to work again too.
// example:
// Create RBD NetworkFence CR for one IP 10.10.10.10
// Created CephFS NetworkFence CR for IP range but above IP comes in the Range
// Delete the CephFS Network Fence CR to unblocklist the IP
// So now the IP (10.10.10.10) is (un)blocklisted and can be used by both protocols.
func (nf *NetworkFence) RemoveNetworkFence(ctx context.Context) error {
	hasBlocklistRangeSupport := true
	// for each CIDR block, convert it into a range of IPs so as to undo blocklisting operation.
	for _, cidr := range nf.Cidr {
		// try range blocklist cmd, if invalid fallback to
		// iterating through IP range.
		if hasBlocklistRangeSupport {
			err := nf.removeCephBlocklist(ctx, cidr, "", true)
			if err == nil {
				continue
			}
			if !strings.Contains(err.Error(), invalidCommandStr) {
				return fmt.Errorf("failed to remove blocklist range %q: %w", cidr, err)
			}
			hasBlocklistRangeSupport = false
		}
		// fetch the list of IPs from a CIDR block
		hosts, err := getIPRange(cidr)
		if err != nil {
			return fmt.Errorf("failed to convert CIDR block %s to corresponding IP range", cidr)
		}
		// remove ceph blocklist for each IP in the range mentioned by the CIDR
		for _, host := range hosts {
			// 0 is used as nonce here to tell ceph
			// to remove the blocklist entry matching: <host>:0/0
			// it is same as telling ceph to remove just the IP
			// without specifying any port or nonce with it.
			err := nf.removeCephBlocklist(ctx, host, "0", false)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (nf *NetworkFence) RemoveClientEviction(ctx context.Context) error {
	// Remove the CIDR block first
	err := nf.RemoveNetworkFence(ctx)
	if err != nil {
		return err
	}

	// Get the ceph blocklist
	blocklist, err := nf.getCephBlocklist(ctx)
	if err != nil {
		return err
	}

	// For each CIDR block, remove the IPs in the blocklist
	// that fall under the CIDR with nonce
	for _, cidr := range nf.Cidr {
		hosts := nf.parseBlocklistForCIDR(ctx, blocklist, cidr)
		log.DebugLog(ctx, "parsed blocklist for CIDR %s: %+v", cidr, hosts)

		for _, host := range hosts {
			err := nf.removeCephBlocklist(ctx, host.IP, host.Nonce, false)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// getCephBlocklist fetches the ceph blocklist and returns it as a string.
func (nf *NetworkFence) getCephBlocklist(ctx context.Context) (string, error) {
	arg := []string{
		"--id", nf.cr.ID,
		"--keyfile=" + nf.cr.KeyFile,
		"-m", nf.Monitors,
	}
	// FIXME: replace the ceph command with go-ceph API in future
	cmd := []string{"osd", "blocklist", "ls"}
	cmd = append(cmd, arg...)
	stdout, stdErr, err := util.ExecCommandWithTimeout(ctx, 2*time.Minute, "ceph", cmd...)
	if err != nil {
		return "", fmt.Errorf("failed to get the ceph blocklist: %w, stderr: %q", err, stdErr)
	}

	return stdout, nil
}

// parseBlocklistEntry parses a single entry from the ceph blocklist
// and returns the IPWithNonce.
func (nf *NetworkFence) parseBlocklistEntry(entry string) IPWithNonce {
	parts := strings.Fields(entry)
	if len(parts) == 0 {
		return IPWithNonce{}
	}

	ipPortNonce := strings.SplitN(parts[0], "/", 2)
	if len(ipPortNonce) != 2 {
		return IPWithNonce{}
	}

	ipPort := ipPortNonce[0]
	nonce := ipPortNonce[1]

	lastColonIndex := strings.LastIndex(ipPortNonce[0], ":")
	if lastColonIndex == -1 {
		return IPWithNonce{}
	}

	if len(ipPort) <= lastColonIndex {
		return IPWithNonce{}
	}
	ip := ipPort[:lastColonIndex]

	return IPWithNonce{IP: ip, Nonce: nonce}
}

// parseBlocklistForCIDR scans the blocklist for the given CIDR and returns
// the list of IPs that lie within the CIDR along with their nonce.
func (nf *NetworkFence) parseBlocklistForCIDR(ctx context.Context, blocklist, cidr string) []IPWithNonce {
	blocklistEntries := strings.Split(blocklist, "\n")

	matchingHosts := make([]IPWithNonce, 0)
	for _, entry := range blocklistEntries {
		entry = strings.TrimSpace(entry)

		// Skip unrelated ranged blocks and invalid entries
		if strings.Contains(entry, "cidr") || !strings.Contains(entry, "/") {
			continue
		}

		blockedHost := nf.parseBlocklistEntry(entry)
		if isIPInCIDR(ctx, blockedHost.IP, cidr) {
			matchingHosts = append(matchingHosts, blockedHost)
		}
	}

	return matchingHosts
}

func ParseClientIP(addr string) (string, error) {
	// Attempt to extract the IP address using a regular expression
	// the regular expression aims to match either a complete IPv6
	// address or a complete IPv4 address follows by any prefix (v1 or v2)
	// if exists
	// (?:v[0-9]+:): this allows for an optional prefix starting with "v"
	// followed by one or more digits and a colon.
	// The ? outside the group makes the entire prefix section optional.
	// (?:[0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}: this allows to check for
	// standard IPv6 address.
	// |: Alternation operator to allow matching either the IPv6 pattern
	// with a prefix or the IPv4 pattern.
	// '(?:\d+\.){3}\d+: This part matches a standard IPv4 address.
	re := regexp.MustCompile(`(?:v[0-9]+:)?([0-9a-fA-F]{1,4}(:[0-9a-fA-F]{1,4}){7}|(?:\d+\.){3}\d+)`)
	ipMatches := re.FindStringSubmatch(addr)

	if len(ipMatches) > 0 {
		ip := net.ParseIP(ipMatches[1])
		if ip != nil {
			return ip.String(), nil
		}
	}

	return "", fmt.Errorf("failed to extract IP address, incorrect format: %s", addr)
}
