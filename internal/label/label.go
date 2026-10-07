package label

const PRD = "prd"

type Def struct {
	Name  string
	Color string
}

func All() []Def {
	return []Def{
		{PRD, "4B6784"},
		{string(Draft), "9AA5B1"},
		{string(Ready), "B98300"},
		{string(Working), "D9692A"},
		{string(Review), "2D955A"},
		{string(Approved), "6E56CF"},
		{string(Stuck), "CC3D38"},
		{string(Light), "9AA5B1"},
		{string(Normal), "6B7785"},
		{string(Heavy), "3D4752"},
	}
}
