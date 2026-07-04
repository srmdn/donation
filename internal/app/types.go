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
	RepoURL     string
	DemoURL     string
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

type Donation struct {
	ID                    int64
	ProjectID             int64
	ProjectTitle          string
	ProjectSlug           string
	DonorName             string
	DonorEmail            string
	Message               string
	Amount                int
	Currency              string
	Status                string
	Visibility            string
	IsSpam                bool
	ModerationNote        string
	Provider              string
	ProviderOrderID       string
	ProviderStatus        string
	ProviderPaymentURL    string
	ProviderPaymentMethod string
	ProviderPaymentNumber string
	ProviderFee           int
	ProviderTotalPayment  int
	ProviderExpiredAt     string
	ProviderCompletedAt   string
	PaidAt                string
	CreatedAt             string
	UpdatedAt             string
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

type ProjectsIndexPageData struct {
	Builder        Builder
	Projects       []Project
	Page           int
	HasPrev        bool
	HasNext        bool
	PrevPage       int
	NextPage       int
	TotalProjects  int
	SupporterCount int
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

type AdminDonationsPageData struct {
	Donations    []Donation
	Error        string
	Notice       string
	TotalCount   int
	PaidCount    int
	PendingCount int
	PublicCount  int
	SpamCount    int
}

type PayPageData struct {
	Builder  Builder
	Donation Donation
}

type ThanksPageData struct {
	Builder  Builder
	Donation Donation
	HasID    bool
}
