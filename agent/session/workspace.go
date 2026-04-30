package session

func newerTimestamp(a, b string) string {
	if b == "" || a >= b {
		return a
	}
	return b
}

func modelSpec(provider, model *string) string {
	if provider == nil || model == nil || *model == "" {
		return ""
	}
	if *provider == "" {
		return *model
	}
	return *provider + "/" + *model
}

func compactionLifecycleStatus(entry summaryEntry) string {
	if entry.ErrorMessage != "" {
		return "errored"
	}
	if entry.Aborted {
		return "aborted"
	}
	return "compacting"
}
