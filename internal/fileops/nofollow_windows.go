package fileops

// Windows has no O_NOFOLLOW. Node mode (the only mode that creates drive files)
// is unsupported on Windows, so this only affects connect-mode relays, which
// keep exactly their previous behavior.
const oNoFollow = 0
