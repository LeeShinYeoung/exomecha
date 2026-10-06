package label

const (
	Problem = "type:problem"
	PRD     = "type:prd"
)

type Def struct {
	Name  string
	Color string
}

func All() []Def {
	return []Def{
		{Problem, "4B6784"},
		{PRD, "4B6784"},
		{string(Queued), "B98300"},
		{string(Working), "D9692A"},
		{string(Review), "2D955A"},
		{string(Stuck), "CC3D38"},
		{string(Light), "9AA5B1"},
		{string(Normal), "6B7785"},
		{string(Heavy), "3D4752"},
	}
}
