package app

type Project struct {
	ID          int64
	Title       string
	Slug        string
	Summary     string
	Description string
	Status      string
	Raised      int
	Goal        int
	Accent      string
	IsActive    bool
	LastUpdated string
}

type TimelineEvent struct {
	Kind    string
	Title   string
	Detail  string
	Amount  int
	TimeAgo string
	Project string
}

type Builder struct {
	Name   string
	Handle string
	Bio    string
}

type PageData struct {
	Builder           Builder
	TotalRaised       int
	SupporterCount    int
	ActiveProjectNum  int
	Projects          []Project
	Timeline          []TimelineEvent
	TimelineHasMore   bool
	TimelineNextLimit int
}

type ProjectPageData struct {
	PageData
	Project Project
}

type AdminLoginPageData struct {
	Error string
}

type AdminProjectsPageData struct {
	Projects    []Project
	Editing     Project
	HasEditing  bool
	Error       string
	Notice      string
	ActiveCount int
}
