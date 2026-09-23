//go:build !darwin && !linux

package lyricsacquisition

import (
	"errors"
	"os"
)

const (
	atomicPublicationProbeDirectoryName = ".moesekai-unsupported-publication-capability-v1"
	atomicPublicationProbeSourceName    = "descriptor-source.unsupported"
	atomicPublicationProbeTargetName    = "descriptor-source.unsupported.published"
)

var (
	errAtomicNamespacePublicationUnsupported = errors.New("HOLD: descriptor-bound lyrics acquisition publication is unsupported on this platform")
	atomicPublicationProbeBody               = []byte("moesekai-unsupported-descriptor-publication-capability-v1\n")
)

func requireAtomicNamespacePublication() error {
	return errAtomicNamespacePublicationUnsupported
}

func preflightAtomicNamespacePublication(*os.File) error {
	return errAtomicNamespacePublicationUnsupported
}

func verifyAtomicNamespacePublicationProbe(*os.File, *os.File, trustedStat) error {
	return errAtomicNamespacePublicationUnsupported
}

func atomicPublishDescriptorNoReplaceAt(*os.File, *os.File, string) error {
	return errAtomicNamespacePublicationUnsupported
}
