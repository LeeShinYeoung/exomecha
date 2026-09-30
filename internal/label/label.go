package label

const (
	Problem = "type:problem"
	PRD     = "type:prd"
	Task    = "type:task"
	// Tasked is added to a PRD once all its Tasks exist, and is never removed.
	Tasked = "tasked"
)

type Def struct {
	Name  string
	Color string
}

func All() []Def {
	return []Def{
		{Problem, "4B6784"},
		{PRD, "4B6784"},
		{Task, "4B6784"},
		{Tasked, "8A5BC6"},
		{string(Pending), "3A7FC2"},
		{string(Queued), "B98300"},
		{string(Working), "D9692A"},
		{string(Review), "2D955A"},
		{string(Stuck), "CC3D38"},
	}
}
