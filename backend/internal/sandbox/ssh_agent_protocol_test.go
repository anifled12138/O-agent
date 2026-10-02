package sandbox

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync/atomic"
	"testing"
)

func TestSSHAgentProxyFiltersIdentitiesAndSignsOnlyLoadedKeys(t *testing.T) {
	keyA := []byte("ssh-key-a-blob")
	keyB := []byte("ssh-key-b-blob")
	allowed := map[string]struct{}{string(keyA): {}}
	client, server := net.Pipe()
	var forwarded atomic.Int32
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSSHAgentProxy(server, func(request []byte) ([]byte, error) {
			forwarded.Add(1)
			switch request[0] {
			case sshAgentRequestIdentities:
				return sshAgentIdentitiesMessage(keyA, keyB), nil
			case sshAgentSignRequest:
				return []byte{sshAgentSignResponse, 0, 0, 0, 3, 's', 'i', 'g'}, nil
			default:
				return nil, io.ErrUnexpectedEOF
			}
		}, allowed)
	}()

	if err := writeSSHAgentFrame(client, []byte{sshAgentRequestIdentities}); err != nil {
		t.Fatal(err)
	}
	identities, err := readSSHAgentFrame(client)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := sshAgentPublicKeys(identities)
	if err != nil || len(keys) != 1 || !bytes.Equal(keys[0], keyA) {
		t.Fatalf("proxy exposed identities outside the selected Host-agent set: keys=%q err=%v", keys, err)
	}
	for _, key := range keys {
		clear(key)
	}
	clear(identities)

	if err := writeSSHAgentFrame(client, sshAgentSignMessage(keyB)); err != nil {
		t.Fatal(err)
	}
	denied, err := readSSHAgentFrame(client)
	if err != nil || !bytes.Equal(denied, []byte{sshAgentFailure}) {
		t.Fatalf("proxy did not reject signing with an unselected key: response=%v err=%v", denied, err)
	}
	clear(denied)

	if err := writeSSHAgentFrame(client, sshAgentSignMessage(keyA)); err != nil {
		t.Fatal(err)
	}
	signed, err := readSSHAgentFrame(client)
	if err != nil || len(signed) == 0 || signed[0] != sshAgentSignResponse {
		t.Fatalf("proxy did not relay the selected-key signature: response=%v err=%v", signed, err)
	}
	clear(signed)
	if forwarded.Load() != 2 {
		t.Fatalf("proxy forwarded %d requests; only identity-list and selected-key signing should reach the Host agent", forwarded.Load())
	}

	_ = client.Close()
	if err := <-serverDone; err == nil {
		t.Fatal("proxy did not close after the command-side pipe ended")
	}
}

func TestSSHAgentProxyRejectsKeyManagementAndMalformedFrames(t *testing.T) {
	client, server := net.Pipe()
	var forwarded atomic.Bool
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- serveSSHAgentProxy(server, func([]byte) ([]byte, error) {
			forwarded.Store(true)
			return []byte{sshAgentSuccess}, nil
		}, map[string]struct{}{})
	}()
	if err := writeSSHAgentFrame(client, []byte{17}); err != nil { // SSH_AGENTC_ADD_IDENTITY
		t.Fatal(err)
	}
	response, err := readSSHAgentFrame(client)
	if err != nil || !bytes.Equal(response, []byte{sshAgentFailure}) {
		t.Fatalf("agent key-management request was not rejected: response=%v err=%v", response, err)
	}
	clear(response)
	if forwarded.Load() {
		t.Fatal("proxy forwarded a key-management request to the Host agent")
	}
	_ = client.Close()
	if err := <-serverDone; err == nil {
		t.Fatal("proxy did not close after the command-side pipe ended")
	}

	if _, err := filterSSHAgentIdentities([]byte{sshAgentIdentitiesAnswer, 0xff, 0xff, 0xff, 0xff}, nil); err == nil {
		t.Fatal("identity response accepted an unreasonable key count")
	}
	if _, err := sshAgentSignKey([]byte{sshAgentSignRequest, 0xff, 0xff, 0xff, 0xff}); err == nil {
		t.Fatal("malformed signing request was accepted")
	}
}

func sshAgentIdentitiesMessage(keys ...[]byte) []byte {
	message := []byte{sshAgentIdentitiesAnswer, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(message[1:5], uint32(len(keys)))
	for index, key := range keys {
		message = appendSSHAgentString(message, key)
		message = appendSSHAgentString(message, []byte{byte('a' + index)})
	}
	return message
}

func sshAgentSignMessage(key []byte) []byte {
	message := []byte{sshAgentSignRequest}
	message = appendSSHAgentString(message, key)
	message = appendSSHAgentString(message, []byte("sign-this"))
	return append(message, 0, 0, 0, 0)
}
