package label

import "slices"

type Status string

const (
	None    Status = ""
	Queued  Status = "status:queued"
	Working Status = "status:working"
	Review  Status = "status:review"
	Stuck   Status = "status:stuck"
)

var statuses = []Status{Queued, Working, Review, Stuck}

var transitions = map[Status][]Status{
	None:    {Queued},
	Queued:  {Working},
	Working: {Review, Queued, Stuck},
	Review:  {Queued, Stuck},
	Stuck:   {Queued, Review},
}

func CanTransition(from, to Status) bool {
	return slices.Contains(transitions[from], to)
}

func IsStatus(name string) bool {
	return slices.Contains(statuses, Status(name))
}
