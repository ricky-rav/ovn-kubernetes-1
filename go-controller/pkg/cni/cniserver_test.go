// +build linux

package cni

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	cnitypes "github.com/containernetworking/cni/pkg/types"
	cni020 "github.com/containernetworking/cni/pkg/types/020"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1listers "k8s.io/client-go/listers/core/v1"
	utiltesting "k8s.io/client-go/util/testing"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
)

func clientDoCNI(t *testing.T, client *http.Client, req *Request) ([]byte, int) {
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("failed to marshal CNI request %v: %v", req, err)
	}

	url := fmt.Sprintf("http://dummy/")
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to send CNI request: %v", err)
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read CNI request response body: %v", err)
	}
	return body, resp.StatusCode
}

var expectedResult cnitypes.Result

func serverHandleCNI(request *PodRequest, podLister corev1listers.PodLister, kclient kubernetes.Interface, kubeAuth *KubeAPIAuth) ([]byte, error) {
	if request.Command == CNIAdd {
		return json.Marshal(&expectedResult)
	} else if request.Command == CNIDel || request.Command == CNIUpdate || request.Command == CNICheck {
		return nil, nil
	}
	return nil, fmt.Errorf("unhandled CNI command %v", request.Command)
}

func makeCNIArgs(namespace, name string) string {
	return fmt.Sprintf("K8S_POD_NAMESPACE=%s;K8S_POD_NAME=%s", namespace, name)
}

const (
	sandboxID    string = "adsfadsfasfdasdfasf"
	namespace    string = "awesome-namespace"
	name         string = "awesome-name"
	cniConfig    string = "{\"cniVersion\": \"0.1.0\",\"name\": \"ovnkube\",\"type\": \"ovnkube\"}"
	cniConfig_40 string = "{\"cniVersion\": \"0.4.0\",\"name\": \"ovnkube\",\"type\": \"ovnkube\"}"
	nodeName     string = "mynode"
)

func TestCNIServer(t *testing.T) {
	tmpDir, err := utiltesting.MkTmpdir("cniserver")
	if err != nil {
		t.Fatalf("failed to create temp directory: %v", err)
	}
	defer os.RemoveAll(tmpDir)
	socketPath := filepath.Join(tmpDir, serverSocketName)
	fakeClient := fake.NewSimpleClientset()

	fakeClientset := &util.OVNClientset{KubeClient: fakeClient}
	wf, err := factory.NewNodeWatchFactory(fakeClientset, nodeName)
	if err != nil {
		t.Fatalf("failed to create watch factory: %v", err)
	}

	s := NewCNIServer(tmpDir, wf)
	if err := s.Start(serverHandleCNI); err != nil {
		t.Fatalf("error starting CNI server: %v", err)
	}

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(proto, addr string) (net.Conn, error) {
				return net.Dial("unix", socketPath)
			},
		},
	}

	expectedIP, expectedNet, _ := net.ParseCIDR("10.0.0.2/24")
	expectedResult = &cni020.Result{
		IP4: &cni020.IPConfig{
			IP: net.IPNet{
				IP:   expectedIP,
				Mask: expectedNet.Mask,
			},
		},
	}

	type testcase struct {
		name        string
		request     *Request
		result      cnitypes.Result
		errorPrefix string
	}

	testcases := []testcase{
		// Normal ADD request
		{
			name: "ADD",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNIAdd),
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig),
			},
			result: expectedResult,
		},
		// Normal DEL request
		{
			name: "DEL",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNIDel),
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig),
			},
			result: nil,
		},
		// Normal UPDATE request
		{
			name: "UPDATE",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNIUpdate),
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig),
			},
			result: nil,
		},
		// Normal CHECK request
		{
			name: "CHECK",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNICheck),
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig_40),
			},
			result: nil,
		},
		// Missing CNI_ARGS
		{
			name: "ARGS1",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNIAdd),
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
				},
				Config: []byte(cniConfig),
			},
			result:      nil,
			errorPrefix: "missing CNI_ARGS",
		},
		// Missing CNI_NETNS
		{
			name: "ARGS2",
			request: &Request{
				Env: map[string]string{
					"CNI_COMMAND":     string(CNIAdd),
					"CNI_CONTAINERID": sandboxID,
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig),
			},
			result:      nil,
			errorPrefix: "missing CNI_NETNS",
		},
		// Missing CNI_COMMAND
		{
			name: "ARGS3",
			request: &Request{
				Env: map[string]string{
					"CNI_CONTAINERID": sandboxID,
					"CNI_NETNS":       "/path/to/something",
					"CNI_ARGS":        makeCNIArgs(namespace, name),
				},
				Config: []byte(cniConfig),
			},
			result:      nil,
			errorPrefix: "unexpected or missing CNI_COMMAND",
		},
	}

	for _, tc := range testcases {
		body, code := clientDoCNI(t, client, tc.request)
		if tc.errorPrefix == "" {
			if code != http.StatusOK {
				t.Fatalf("[%s] expected status %v but got %v", tc.name, http.StatusOK, code)
			}
			if tc.result != nil {
				result := &cni020.Result{}
				if err := json.Unmarshal(body, result); err != nil {
					t.Fatalf("[%s] failed to unmarshal response '%s': %v", tc.name, string(body), err)
				}
				if !reflect.DeepEqual(result, tc.result) {
					t.Fatalf("[%s] expected result %v but got %v", tc.name, tc.result, result)
				}
			}
		} else {
			if code != http.StatusBadRequest {
				t.Fatalf("[%s] expected status %v but got %v", tc.name, http.StatusBadRequest, code)
			}
			if !strings.HasPrefix(string(body), tc.errorPrefix) {
				t.Fatalf("[%s] unexpected error message '%v'", tc.name, string(body))
			}
		}
	}
}
