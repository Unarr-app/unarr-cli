//go:build windows

package postprocess

// Windows has no O_NOFOLLOW: CreateFile follows reparse points unless opened
// with FILE_FLAG_OPEN_REPARSE_POINT, which os.OpenFile does not expose.
//
// The residual risk is smaller than on unix but NOT zero, and it is worth
// stating plainly rather than implying parity:
//   - Archive members that ARE symlinks are refused outright (checkMode), so
//     extraction never plants the link itself.
//   - Creating a symlink on Windows requires SeCreateSymbolicLinkPrivilege
//     (admin, or Developer Mode), so an unprivileged attacker cannot pre-plant
//     one either.
//   - safeWriter.resolve still verifies that the resolved PARENT directory stays
//     inside the destination, which catches a directory junction planted there
//     by some other means — the realistic Windows variant of this attack.
//
// What remains uncovered is a symlink or reparse point placed at the exact
// final path by a privileged process between resolve() and OpenFile(). That is
// a TOCTOU window requiring existing admin rights on the box.
const noFollowFlag = 0
