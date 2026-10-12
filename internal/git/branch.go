package git

func (g *Git) Base() string { return g.base }

func (g *Git) BaseRef() string { return g.RemoteRef(g.base) }

func (g *Git) RemoteRef(branch string) string { return "origin/" + branch }
