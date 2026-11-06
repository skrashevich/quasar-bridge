package sound

// Control identifies a native player operation independently of spoken text.
type Control string

const (
	ControlResume   Control = "resume"
	ControlNext     Control = "next"
	ControlPrevious Control = "previous"
)
