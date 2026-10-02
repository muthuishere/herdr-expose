package chat

import "os/exec"

// execLookPath is a seam so a test can assert the "not installed" path without
// depending on what happens to be on the developer's machine.
var execLookPath = exec.LookPath
