package app

type Project struct {
	ID            int64
	Title         string
	Slug          string
	Summary       string
	Description   string
	Status        string
	Raised        int
	Goal          int
	Accent        string
	RepoURL       string
	DemoURL       string
	DeadlineDate  string
	DeadlineText  string
	DeadlineEnded bool
	IsActive      bool
	LastUpdated   string
}

type TimelineEvent struct {
	Kind    string
	Title   string
	Detail  string
	Amount  int
	TimeAgo string
	Project string
}

type ProjectUpdate struct {
	ID           int64
	ProjectID    int64
	ProjectSlug  string
	ProjectTitle string
	Title        string
	Body         string
	PublishedAt  string
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
	IsTest                bool
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
	SettlementSource      string
	ManualReference       string
	PaidAt                string
	CreatedAt             string
	UpdatedAt             string
}

type ManualDonationInput struct {
	ProjectID       int64
	DonorName       string
	DonorEmail      string
	Message         string
	Amount          int
	PaidAt          string
	Visibility      string
	ManualReference string
	ModerationNote  string
}

type Builder struct {
	Name       string
	Handle     string
	Bio        string
	AvatarURL  string
	WebsiteURL string
	GitHubURL  string
	GitLabURL  string
}

type MetaData struct {
	Title        string
	Description  string
	CanonicalURL string
	ImageURL     string
	SiteName     string
	Type         string
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
	CSRFToken         string
	Meta              MetaData
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
	Meta           MetaData
}

type AdminLoginPageData struct {
	Error     string
	Notice    string
	Email     string
	CSRFToken string
}

type AdminLoginVerifyPageData struct {
	Error     string
	Token     string
	CSRFToken string
}

type AdminProjectsPageData struct {
	Projects    []Project
	Editing     Project
	HasEditing  bool
	Error       string
	Notice      string
	ActiveCount int
	CSRFToken   string
}

type AdminUpdatesPageData struct {
	Projects        []Project
	Updates         []ProjectUpdate
	Error           string
	Notice          string
	UpdateEditingID int64
	UpdateProjectID int64
	UpdateTitle     string
	UpdateBody      string
	CSRFToken       string
}

type AdminDonationsPageData struct {
	Donations         []Donation
	Projects          []Project
	Error             string
	Notice            string
	TotalCount        int
	PaidCount         int
	PendingCount      int
	PublicCount       int
	SpamCount         int
	TestCount         int
	FilterStatus      string
	FilterVisibility  string
	FilterSpam        string
	FilterTest        string
	FilterProjectSlug string
	FilterHasActive   bool
	SearchQuery       string
	ManualPaidAt      string
	CSRFToken         string
}

type PayPageData struct {
	Builder   Builder
	Donation  Donation
	CSRFToken string
}

type ThanksPageData struct {
	Builder  Builder
	Donation Donation
	HasID    bool
}
