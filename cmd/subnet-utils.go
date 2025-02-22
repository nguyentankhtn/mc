// Copyright (c) 2015-2021 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package cmd

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/minio/cli"
	"github.com/minio/madmin-go"
	"github.com/minio/mc/pkg/probe"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	subnetRespBodyLimit  = 1 << 20 // 1 MiB
	minioSubscriptionURL = "https://min.io/subscription"
)

var subnetCommonFlags = []cli.Flag{
	cli.StringFlag{
		Name:  "name",
		Usage: "Specify the name to associate to this MinIO cluster in SUBNET",
	},
	cli.StringFlag{
		Name:  "subnet-proxy",
		Usage: "Specify the HTTP(S) proxy URL to use for connecting to SUBNET",
	},
	cli.BoolFlag{
		Name:  "airgap",
		Usage: "Use in environments without network access to SUBNET (e.g. airgapped, firewalled, etc.)",
	},
	cli.BoolFlag{
		Name:   "dev",
		Usage:  "Development mode - talks to local SUBNET",
		Hidden: true,
	},
	cli.BoolFlag{
		// Deprecated Oct 2021. Same as airgap, retaining as hidden for backward compatibility
		Name:   "offline",
		Usage:  "Use in environments without network access to SUBNET (e.g. airgapped, firewalled, etc.)",
		Hidden: true,
	},
}

func subnetBaseURL() string {
	if globalDevMode {
		return "http://localhost:9000"
	}

	return "https://subnet.min.io"
}

func subnetHealthUploadURL() string {
	return subnetBaseURL() + "/api/health/upload"
}

func subnetRegisterURL() string {
	return subnetBaseURL() + "/api/cluster/register"
}

func subnetLoginURL() string {
	return subnetBaseURL() + "/api/auth/login"
}

func subnetOrgsURL() string {
	return subnetBaseURL() + "/api/auth/organizations"
}

func subnetMFAURL() string {
	return subnetBaseURL() + "/api/auth/mfa-login"
}

func checkURLReachable(url string) *probe.Error {
	clnt := httpClient(10 * time.Second)
	req, e := http.NewRequest(http.MethodHead, url, nil)
	if e != nil {
		return probe.NewError(e).Trace(url)
	}
	resp, e := clnt.Do(req)
	if e != nil {
		return probe.NewError(e).Trace(url)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return probe.NewError(errors.New(resp.Status)).Trace(url)
	}
	return nil
}

func subnetURLWithAuth(reqURL string, apiKey string, license string) (string, map[string]string, error) {
	headers := map[string]string{}
	if len(apiKey) > 0 {
		// Add api key in url for authentication
		reqURL = reqURL + "?api_key=" + apiKey
	} else if len(license) > 0 {
		// Add license in url for authentication
		reqURL = reqURL + "?license=" + license
	} else {
		// API key not available in minio/mc config.
		// Ask the user to log in to get auth token
		token, e := subnetLogin()
		if e != nil {
			return "", nil, e
		}
		headers = subnetAuthHeaders(token)

		accID, err := getSubnetAccID(headers)
		if err != nil {
			return "", headers, e
		}

		reqURL = reqURL + "?aid=" + accID
	}
	return reqURL, headers, nil
}

func subnetAuthHeaders(authToken string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + authToken}
}

func httpDo(req *http.Request) (*http.Response, error) {
	client := httpClient(10 * time.Second)
	if globalSubnetProxyURL != nil {
		client.Transport.(*http.Transport).Proxy = http.ProxyURL(globalSubnetProxyURL)
	}
	return client.Do(req)
}

func subnetReqDo(r *http.Request, headers map[string]string) (string, error) {
	for k, v := range headers {
		r.Header.Add(k, v)
	}

	ct := r.Header.Get("Content-Type")
	if len(ct) == 0 {
		r.Header.Add("Content-Type", "application/json")
	}

	resp, e := httpDo(r)
	if e != nil {
		return "", e
	}

	defer resp.Body.Close()
	respBytes, e := ioutil.ReadAll(io.LimitReader(resp.Body, subnetRespBodyLimit))
	if e != nil {
		return "", e
	}
	respStr := string(respBytes)

	if resp.StatusCode == http.StatusOK {
		return respStr, nil
	}
	return respStr, fmt.Errorf("Request failed with code %d and error: %s", resp.StatusCode, respStr)
}

func subnetGetReq(reqURL string, headers map[string]string) (string, error) {
	r, e := http.NewRequest(http.MethodGet, reqURL, nil)
	if e != nil {
		return "", e
	}
	return subnetReqDo(r, headers)
}

func subnetPostReq(reqURL string, payload interface{}, headers map[string]string) (string, error) {
	body, e := json.Marshal(payload)
	if e != nil {
		return "", e
	}
	r, e := http.NewRequest(http.MethodPost, reqURL, bytes.NewReader(body))
	if e != nil {
		return "", e
	}
	return subnetReqDo(r, headers)
}

func getSubnetKeyFromMinIOConfig(alias string, key string) (bool, string) {
	client, err := newAdminClient(alias)
	fatalIf(err, "Unable to initialize admin connection.")

	if minioConfigSupportsSubnet(client) {
		sh, pe := client.HelpConfigKV(globalContext, "subnet", "", false)
		fatalIf(probe.NewError(pe), "Unable to get config keys for SUBNET")

		buf, e := client.GetConfigKV(globalContext, "subnet")
		fatalIf(probe.NewError(e), "Unable to get server SUBNET config")

		tgt, e := madmin.ParseSubSysTarget(buf, sh)
		fatalIf(probe.NewError(e), "Unable to parse sub-system target 'subnet'")

		for _, kv := range tgt.KVS {
			if kv.Key == key {
				return true, kv.Value
			}
		}
	}
	return false, ""
}

func getSubnetAPIKeyFromConfig(alias string) string {
	// get the subnet api_key config from MinIO if available
	supported, apiKey := getSubnetKeyFromMinIOConfig(alias, "api_key")
	if supported {
		return apiKey
	}

	// otherwise get it from mc config
	return mcConfig().Aliases[alias].APIKey
}

func getSubnetLicenseFromConfig(alias string) string {
	// get the subnet license config from MinIO if available
	supported, lic := getSubnetKeyFromMinIOConfig(alias, "license")
	if supported {
		return lic
	}

	// otherwise get it from mc config
	return mcConfig().Aliases[alias].License
}

func mcConfig() *configV10 {
	loadMcConfig = loadMcConfigFactory()
	config, err := loadMcConfig()
	fatalIf(err.Trace(mustGetMcConfigPath()), "Unable to access configuration file.")
	return config
}

func minioConfigSupportsSubnet(client *madmin.AdminClient) bool {
	help, e := client.HelpConfigKV(globalContext, "", "", false)
	fatalIf(probe.NewError(e), "Unable to get minio config keys")

	for _, h := range help.KeysHelp {
		if h.Key == "subnet" {
			return true
		}
	}

	return false
}

func setSubnetAPIKeyConfig(alias string, apiKey string) {
	supported, _ := getSubnetKeyFromMinIOConfig(alias, "api_key")
	if supported {
		// Create a new MinIO Admin Client
		client, err := newAdminClient(alias)
		fatalIf(err, "Unable to initialize admin connection.")

		configStr := "subnet license= api_key=" + apiKey
		_, e := client.SetConfigKV(globalContext, configStr)
		fatalIf(probe.NewError(e), "Unable to set SUBNET API key config on minio")
		return
	}
	mcCfg := mcConfig()
	aliasCfg := mcCfg.Aliases[alias]
	aliasCfg.APIKey = apiKey
	setAlias(alias, aliasCfg)
}

func getClusterRegInfo(admInfo madmin.InfoMessage, clusterName string) ClusterRegistrationInfo {
	noOfPools := 1
	noOfDrives := 0
	for _, srvr := range admInfo.Servers {
		if srvr.PoolNumber > noOfPools {
			noOfPools = srvr.PoolNumber
		}
		noOfDrives += len(srvr.Disks)
	}

	totalSpace, usedSpace := getDriveSpaceInfo(admInfo)

	return ClusterRegistrationInfo{
		DeploymentID: admInfo.DeploymentID,
		ClusterName:  clusterName,
		UsedCapacity: admInfo.Usage.Size,
		Info: ClusterInfo{
			MinioVersion:    admInfo.Servers[0].Version,
			NoOfServerPools: noOfPools,
			NoOfServers:     len(admInfo.Servers),
			NoOfDrives:      noOfDrives,
			TotalDriveSpace: totalSpace,
			UsedDriveSpace:  usedSpace,
			NoOfBuckets:     admInfo.Buckets.Count,
			NoOfObjects:     admInfo.Objects.Count,
		},
	}
}

func getDriveSpaceInfo(admInfo madmin.InfoMessage) (uint64, uint64) {
	total := uint64(0)
	used := uint64(0)
	for _, srvr := range admInfo.Servers {
		for _, d := range srvr.Disks {
			total += d.TotalSpace
			used += d.UsedSpace
		}
	}
	return total, used
}

func generateRegToken(clusterRegInfo ClusterRegistrationInfo) (string, error) {
	token, e := json.Marshal(clusterRegInfo)
	if e != nil {
		return "", e
	}

	return base64.StdEncoding.EncodeToString(token), nil
}

func subnetLogin() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("SUBNET username: ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)

	if len(username) == 0 {
		return "", errors.New("Username cannot be empty. If you don't have one, please create one from here: " + minioSubscriptionURL)
	}

	fmt.Print("Password: ")
	bytepw, _ := terminal.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()

	loginReq := map[string]string{
		"username": username,
		"password": string(bytepw),
	}
	respStr, e := subnetPostReq(subnetLoginURL(), loginReq, nil)
	if e != nil {
		return "", e
	}

	mfaRequired := gjson.Get(respStr, "mfa_required").Bool()
	if mfaRequired {
		mfaToken := gjson.Get(respStr, "mfa_token").String()
		fmt.Print("OTP received in email: ")
		byteotp, _ := terminal.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()

		mfaLoginReq := SubnetMFAReq{Username: username, OTP: string(byteotp), Token: mfaToken}
		respStr, e = subnetPostReq(subnetMFAURL(), mfaLoginReq, nil)
		if e != nil {
			return "", e
		}
	}

	token := gjson.Get(respStr, "token_info.access_token")
	if token.Exists() {
		return token.String(), nil
	}
	return "", fmt.Errorf("access token not found in response")
}

func getSubnetAccID(headers map[string]string) (string, error) {
	respStr, e := subnetGetReq(subnetOrgsURL(), headers)
	if e != nil {
		return "", e
	}
	data := gjson.Parse(respStr)
	orgs := data.Array()
	idx := 1
	if len(orgs) > 1 {
		fmt.Println("You are part of multiple organizations on SUBNET:")
		for idx, org := range orgs {
			fmt.Println("  ", idx+1, ":", org.Get("company"))
		}
		fmt.Print("Please choose the organization for this cluster: ")
		reader := bufio.NewReader(os.Stdin)
		accIdx, _ := reader.ReadString('\n')
		accIdx = strings.Trim(accIdx, "\n")
		idx, e = strconv.Atoi(accIdx)
		if e != nil {
			return "", e
		}
		if idx > len(orgs) {
			msg := "Invalid choice for organization. Please run the command again."
			return "", fmt.Errorf(msg)
		}
	}
	return orgs[idx-1].Get("accountId").String(), nil
}

// registerClusterOnSubnet - Registers the given cluster on SUBNET
func registerClusterOnSubnet(alias string, clusterRegInfo ClusterRegistrationInfo) (string, error) {
	apiKey := getSubnetAPIKeyFromConfig(alias)

	lic := ""
	if len(apiKey) == 0 {
		lic = getSubnetLicenseFromConfig(alias)
	}

	regURL, headers, e := subnetURLWithAuth(subnetRegisterURL(), apiKey, lic)
	if e != nil {
		return "", e
	}

	regToken, e := generateRegToken(clusterRegInfo)
	if e != nil {
		return "", e
	}

	reqPayload := ClusterRegistrationReq{Token: regToken}
	return subnetPostReq(regURL, reqPayload, headers)
}

// extractAndSaveAPIKey - extract api key from response and set it in minio config
func extractAndSaveAPIKey(alias string, resp string) {
	subnetAPIKey := gjson.Parse(resp).Get("api_key").String()
	if len(subnetAPIKey) > 0 {
		setSubnetAPIKeyConfig(alias, subnetAPIKey)
	}
}
