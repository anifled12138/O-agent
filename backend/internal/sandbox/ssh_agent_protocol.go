package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	sshAgentRequestIdentities = 11
	sshAgentIdentitiesAnswer  = 12
	sshAgentSignRequest       = 13
	sshAgentSignResponse      = 14
	sshAgentFailure           = 5
	sshAgentSuccess           = 6
	sshAgentExtension         = 27
	maxSSHAgentFrame          = 256 << 10
	maxSSHAgentIdentities     = 4096
)

func readSSHAgentFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxSSHAgentFrame {
		return nil, fmt.Errorf("SSH agent frame length %d is outside the allowed range", length)
	}
	message := make([]byte, int(length))
	if _, err := io.ReadFull(reader, message); err != nil {
		clear(message)
		return nil, err
	}
	return message, nil
}

func writeSSHAgentFrame(writer io.Writer, message []byte) error {
	if len(message) == 0 || len(message) > maxSSHAgentFrame {
		return errors.New("SSH agent response is outside the allowed size")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(message)))
	if err := writeSSHAgentAll(writer, header[:]); err != nil {
		return err
	}
	return writeSSHAgentAll(writer, message)
}

func writeSSHAgentAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		written, err := writer.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		value = value[written:]
	}
	return nil
}

func sshAgentPublicKeys(message []byte) ([][]byte, error) {
	if len(message) == 1 && message[0] == sshAgentFailure {
		return nil, nil
	}
	if len(message) < 5 || message[0] != sshAgentIdentitiesAnswer {
		return nil, errors.New("SSH agent returned an invalid identities response")
	}
	count := binary.BigEndian.Uint32(message[1:5])
	if count > maxSSHAgentIdentities {
		return nil, errors.New("SSH agent returned too many identities")
	}
	position := 5
	keys := make([][]byte, 0, count)
	for index := uint32(0); index < count; index++ {
		key, next, err := readSSHAgentString(message, position)
		if err != nil || len(key) == 0 {
			return nil, errors.Join(errors.New("SSH agent identity key is malformed"), err)
		}
		comment, next, err := readSSHAgentString(message, next)
		clear(comment)
		if err != nil {
			return nil, errors.Join(errors.New("SSH agent identity comment is malformed"), err)
		}
		keys = append(keys, append([]byte(nil), key...))
		position = next
	}
	if position != len(message) {
		for _, key := range keys {
			clear(key)
		}
		return nil, errors.New("SSH agent identities response has trailing data")
	}
	return keys, nil
}

func filterSSHAgentIdentities(message []byte, allowed map[string]struct{}) ([]byte, error) {
	keys, err := sshAgentPublicKeys(message)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, key := range keys {
			clear(key)
		}
	}()
	if len(message) == 1 && message[0] == sshAgentFailure {
		return []byte{sshAgentFailure}, nil
	}
	filtered := make([][]byte, 0, len(keys))
	position := 5
	for _, key := range keys {
		_, next, err := readSSHAgentString(message, position)
		if err != nil {
			return nil, err
		}
		comment, end, err := readSSHAgentString(message, next)
		if err != nil {
			return nil, err
		}
		if _, ok := allowed[string(key)]; ok {
			item := make([]byte, 0, 8+len(key)+len(comment))
			item = appendSSHAgentString(item, key)
			item = appendSSHAgentString(item, comment)
			filtered = append(filtered, item)
		}
		position = end
	}
	response := []byte{sshAgentIdentitiesAnswer, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(response[1:5], uint32(len(filtered)))
	for _, item := range filtered {
		response = append(response, item...)
		clear(item)
	}
	return response, nil
}

func sshAgentSignKey(message []byte) ([]byte, error) {
	if len(message) < 1 || message[0] != sshAgentSignRequest {
		return nil, errors.New("SSH agent request is not a sign request")
	}
	key, position, err := readSSHAgentString(message, 1)
	if err != nil || len(key) == 0 {
		return nil, errors.Join(errors.New("SSH agent sign request has no public key"), err)
	}
	data, position, err := readSSHAgentString(message, position)
	clear(data)
	if err != nil || len(message)-position != 4 {
		return nil, errors.Join(errors.New("SSH agent sign request is malformed"), err)
	}
	return append([]byte(nil), key...), nil
}

func sshAgentExtensionName(message []byte) (string, bool) {
	if len(message) < 1 || message[0] != sshAgentExtension {
		return "", false
	}
	name, _, err := readSSHAgentString(message, 1)
	if err != nil {
		return "", false
	}
	return string(name), true
}

func readSSHAgentString(message []byte, offset int) ([]byte, int, error) {
	if offset < 0 || len(message)-offset < 4 {
		return nil, offset, io.ErrUnexpectedEOF
	}
	length := binary.BigEndian.Uint32(message[offset : offset+4])
	start := offset + 4
	if length > maxSSHAgentFrame || uint64(length) > uint64(len(message)-start) {
		return nil, offset, errors.New("SSH agent string length is invalid")
	}
	end := start + int(length)
	return message[start:end], end, nil
}

func appendSSHAgentString(target, value []byte) []byte {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	target = append(target, size[:]...)
	return append(target, value...)
}

// serveSSHAgentProxy permits identity enumeration, signatures from keys that
// were already loaded in the Host agent when the command began, and OpenSSH's
// host-binding extension. It never forwards key-management operations.
func serveSSHAgentProxy(client io.ReadWriteCloser, forward func([]byte) ([]byte, error), allowed map[string]struct{}) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, client.Close()) }()
	for {
		request, err := readSSHAgentFrame(client)
		if err != nil {
			return err
		}
		var response []byte
		switch {
		case len(request) == 1 && request[0] == sshAgentRequestIdentities:
			upstream, forwardErr := forward(request)
			if forwardErr != nil {
				clear(upstream)
				response = []byte{sshAgentFailure}
			} else {
				response, err = filterSSHAgentIdentities(upstream, allowed)
				clear(upstream)
			}
		case len(request) > 0 && request[0] == sshAgentSignRequest:
			var key []byte
			key, err = sshAgentSignKey(request)
			if err == nil {
				if _, ok := allowed[string(key)]; !ok {
					response = []byte{sshAgentFailure}
				} else {
					response, err = forward(request)
					if err == nil && (len(response) == 0 || (response[0] != sshAgentSignResponse && response[0] != sshAgentFailure)) {
						clear(response)
						response = nil
						err = errors.New("SSH agent returned an invalid signature response")
					}
				}
			}
			clear(key)
		case len(request) > 0 && request[0] == sshAgentExtension:
			if name, ok := sshAgentExtensionName(request); ok && name == "session-bind@openssh.com" {
				response, err = forward(request)
				if err == nil && (len(response) == 0 || (response[0] != sshAgentSuccess && response[0] != sshAgentFailure)) {
					clear(response)
					response = nil
					err = errors.New("SSH agent returned an invalid extension response")
				}
			} else {
				response = []byte{sshAgentFailure}
			}
		default:
			response = []byte{sshAgentFailure}
		}
		clear(request)
		if err != nil {
			clear(response)
			return err
		}
		writeErr := writeSSHAgentFrame(client, response)
		clear(response)
		if writeErr != nil {
			return writeErr
		}
	}
}

func forwardSSHAgentRequest(agent io.ReadWriteCloser, request []byte) ([]byte, error) {
	if len(request) == 0 || len(request) > maxSSHAgentFrame {
		return nil, errors.New("SSH agent request is outside the allowed size")
	}
	if err := writeSSHAgentFrame(agent, request); err != nil {
		return nil, err
	}
	return readSSHAgentFrame(agent)
}
