//go:build !linux

package projectquota

func ProbeFilesystem(string) (ProbeResult, error) {
	return ProbeResult{Reason: "project quota probing is supported only on Linux cloud workers"}, nil
}
