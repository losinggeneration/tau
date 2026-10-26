package plugins

import "fmt"

func pluginDiscoveryHash(pkgDir, manifestPath string, manifest *Manifest, binaryPath string, fileHash func(string) (string, error)) (string, error) {
	if manifestNeedsManagedShim(manifest) {
		return computeManagedShimHash(manifestPath, manifest)
	}
	if manifest != nil && manifest.HasRunCommand() {
		return computeDirHash(pkgDir)
	}
	binaryHash, err := fileHash(binaryPath)
	if err != nil {
		return "", err
	}
	if manifest == nil || manifestPath == "" {
		return binaryHash, nil
	}
	manifestHash, err := fileHash(manifestPath)
	if err != nil {
		return "", fmt.Errorf("hash plugin manifest: %w", err)
	}
	return binaryHash + ":" + manifestHash, nil
}
