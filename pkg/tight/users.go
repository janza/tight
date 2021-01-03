package tight

import (
	"context"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// Users wraps the user list with convenient operations and cache.
type Users struct {
	users       map[string]slack.User
	mu          sync.Mutex
	pagination  int
	slackClient *slack.Client
}

// NewUsers creates a new Users object
func NewUsers(slackClient *slack.Client, pagination int) *Users {
	return &Users{
		users:       make(map[string]slack.User),
		pagination:  pagination,
		slackClient: slackClient,
	}
}

// Fetch retrieves all the users on a given Slack team. The Slack client has to
// be valid and connected.
func (u *Users) Fetch(client *slack.Client) error {
	var opts []slack.GetUsersOption
	if u.pagination > 0 {
		// log.Debugf("Setting user pagination to %d", u.pagination)
		opts = append(opts, slack.GetUsersOptionLimit(u.pagination))
	}
	up := u.slackClient.GetUsersPaginated(opts...)
	var (
		err   error
		ctx   = context.Background()
		users = make(map[string]slack.User)
	)
	// start := time.Now()
	for err == nil {
		up, err = up.Next(ctx)
		if err == nil {
			for _, u := range up.Users {
				users[u.ID] = u
			}
		} else if rateLimitedError, ok := err.(*slack.RateLimitedError); ok {
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(rateLimitedError.RetryAfter):
				err = nil
			}
		}
	}
	err = up.Failure(err)
	if err != nil {
	}
	u.mu.Lock()
	u.users = users
	u.mu.Unlock()
	return nil
}

// Count returns the number of users. This method must be called after `Fetch`.
func (u *Users) Count() int {
	return len(u.users)
}

// ByID retrieves a user by its Slack ID.
func (u *Users) ByID(id string) *slack.User {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, u := range u.users {
		if u.ID == id {
			return &u
		}
	}
	user, err := u.slackClient.GetUserInfo(id)
	if err != nil {
		return nil
	}
	return user
}

// ByName retrieves a user by its Slack name.
func (u *Users) ByName(name string) *slack.User {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, u := range u.users {
		if u.Name == name {
			return &u
		}
	}
	return nil
}

// IDsToNames returns a list of user names from the given IDs. The
// returned list could be shorter if there are invalid user IDs.
// Warning: this method is probably only useful for NAMES commands
// where a non-exact mapping is acceptable.
func (u *Users) IDsToNames(userIDs ...string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	names := make([]string, 0)
	for _, uid := range userIDs {
		if u, ok := u.users[uid]; ok {
			names = append(names, u.Name)
		} else {
			// log.Warningf("Unknown user ID %s", uid)
		}
	}
	return names
}
