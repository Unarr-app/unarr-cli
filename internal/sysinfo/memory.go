package sysinfo

// TotalMemory reports how much physical memory this process can use: the host's
// installed RAM, lowered to the cgroup memory limit when the process runs in a
// container that sets one (Linux). ok is false when it cannot be determined;
// callers must then fall back to a conservative default, never assume zero.
func TotalMemory() (bytes uint64, ok bool) {
	return platformTotalMemory()
}
