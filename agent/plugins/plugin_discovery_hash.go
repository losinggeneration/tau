package plugins

func pluginDiscoveryHash(pkgDir, manifestPath string, manifest *Manifest, binaryPath string, fileHash func(string) (string, error)) (string, error) {
	if manifestNeedsManagedShim(manifest) {
		return computeManagedShimHash(manifestPath, manifest)
	}
	if manifest != nil && manifest.HasRunCommand() {
		return computeDirHash(pkgDir)
	}
	return fileHash(binaryPath)
}
