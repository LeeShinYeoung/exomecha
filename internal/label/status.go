package label

import "slices"

type Status string

const (
	None     Status = ""
	Draft    Status = "status:draft"
	Ready    Status = "status:ready"
	Working  Status = "status:working"
	Review   Status = "status:review"
	Approved Status = "status:approved"
	Stuck    Status = "status:stuck"
)

// Draft counts as a status, so a PRD a person labeled draft and ready at once is left to them.
var statuses = []Status{Draft, Ready, Working, Review, Approved, Stuck}

// No worker moves a PRD out of none or draft. Only a person makes it ready.
var transitions = map[Status][]Status{
	// Stuck: a PRD only a person can fix, such as one that names no repository.
	Ready:    {Working, Stuck},
	Working:  {Review, Ready, Stuck},
	Review:   {Ready, Approved, Stuck},
	Approved: {Review, Stuck},
	Stuck:    {Ready, Review},
}

func CanTransition(from, to Status) bool {
	return slices.Contains(transitions[from], to)
}

func IsStatus(name string) bool {
	return slices.Contains(statuses, Status(name))
}
