package main

// choice is one picked image and where it came from.
type choice struct {
	art  artwork
	ok   bool
	aura bool // an AURA pick rather than creator priority
}

func (p *auraPin) posterID() string {
	if p == nil {
		return ""
	}
	return p.Poster
}

func (p *auraPin) backdropID() string {
	if p == nil {
		return ""
	}
	return p.Backdrop
}

func (p *auraPin) seasonID(season int) string {
	if p == nil {
		return ""
	}
	return p.Seasons[season]
}

func (p *auraPin) cardID(season, episode int) string {
	if p == nil {
		return ""
	}
	return p.Cards[[2]int{season, episode}]
}

// choosePoster uses the AURA pick when there is one, else creator priority.
func choosePoster(b bundle, cfg settings, language string, pin *auraPin) choice {
	if a, ok := pinned(b, pin.posterID()); ok {
		return choice{a, true, true}
	}
	a, ok := best(b, cfg, kindPoster, language)
	return choice{a, ok, false}
}

// chooseWithPoster picks a background or logo, preferring the poster's set.
func chooseWithPoster(b bundle, cfg settings, kind, language string, poster choice, pinID string) choice {
	if a, ok := pinned(b, pinID); ok {
		return choice{a, true, true}
	}
	set := ""
	if poster.ok {
		set = poster.art.SetID
	}
	a, ok := pickInSet(b, cfg, kind, 0, 0, language, setChoice{ID: set})
	return choice{a, ok, false}
}
