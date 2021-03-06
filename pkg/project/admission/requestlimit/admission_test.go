package requestlimit

import (
	"bytes"
	"testing"

	"k8s.io/kubernetes/pkg/admission"
	kapi "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/client/cache"
	ktestclient "k8s.io/kubernetes/pkg/client/unversioned/testclient"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"

	"github.com/openshift/origin/pkg/client/testclient"
	oadmission "github.com/openshift/origin/pkg/cmd/server/admission"
	projectapi "github.com/openshift/origin/pkg/project/api"
	projectcache "github.com/openshift/origin/pkg/project/cache"
	userapi "github.com/openshift/origin/pkg/user/api"
	apierrors "k8s.io/kubernetes/pkg/api/errors"
)

func TestReadConfig(t *testing.T) {

	tests := []struct {
		config      string
		expected    ProjectRequestLimitConfig
		errExpected bool
	}{
		{
			// multiple selectors
			config: `apiVersion: v1
kind: ProjectRequestLimitConfig
limits:
- selector:
    level:
      platinum
- selector:
    level:
      gold
  maxProjects: 500
- selector:
    level:
      silver
  maxProjects: 100
- selector:
    level:
      bronze
  maxProjects: 20
- selector: {}
  maxProjects: 1
`,
			expected: ProjectRequestLimitConfig{
				Limits: []ProjectLimitBySelector{
					{
						Selector:    map[string]string{"level": "platinum"},
						MaxProjects: nil,
					},
					{
						Selector:    map[string]string{"level": "gold"},
						MaxProjects: intp(500),
					},
					{
						Selector:    map[string]string{"level": "silver"},
						MaxProjects: intp(100),
					},
					{
						Selector:    map[string]string{"level": "bronze"},
						MaxProjects: intp(20),
					},
					{
						Selector:    map[string]string{},
						MaxProjects: intp(1),
					},
				},
			},
		},
		{
			// single selector
			config: `apiVersion: v1
kind: ProjectRequestLimitConfig
limits:
- maxProjects: 1
`,
			expected: ProjectRequestLimitConfig{
				Limits: []ProjectLimitBySelector{
					{
						Selector:    nil,
						MaxProjects: intp(1),
					},
				},
			},
		},
		{
			// no selectors
			config: `apiVersion: v1
kind: ProjectRequestLimitConfig
`,
			expected: ProjectRequestLimitConfig{},
		},
	}

	for n, tc := range tests {
		cfg, err := readConfig(bytes.NewBufferString(tc.config))
		if err != nil && !tc.errExpected {
			t.Errorf("%d: unexpected error: %v", n, err)
			continue
		}
		if err == nil && tc.errExpected {
			t.Errorf("%d: expected error, got none", n)
			continue
		}
		if !configEquals(cfg, &tc.expected) {
			t.Errorf("%d: unexpected result. Got %#v. Expected %#v", n, cfg, tc.expected)
		}
	}
}

func TestMaxProjectByRequester(t *testing.T) {
	tests := []struct {
		userLabels      map[string]string
		expectUnlimited bool
		expectedLimit   int
	}{
		{
			userLabels:      map[string]string{"platinum": "yes"},
			expectUnlimited: true,
		},
		{
			userLabels:    map[string]string{"gold": "yes"},
			expectedLimit: 10,
		},
		{
			userLabels:    map[string]string{"silver": "yes", "bronze": "yes"},
			expectedLimit: 3,
		},
		{
			userLabels:    map[string]string{"unknown": "yes"},
			expectedLimit: 1,
		},
	}

	for _, tc := range tests {
		reqLimit, err := NewProjectRequestLimit(multiLevelConfig())
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		user := fakeUser("testuser", tc.userLabels)
		client := testclient.NewSimpleFake(user)
		reqLimit.(oadmission.WantsOpenshiftClient).SetOpenshiftClient(client)

		maxProjects, hasLimit, err := reqLimit.(*projectRequestLimit).maxProjectsByRequester("testuser")
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}

		if tc.expectUnlimited {

			if hasLimit {
				t.Errorf("Expected no limit, but got limit for labels %v", tc.userLabels)
			}
			continue
		}
		if !tc.expectUnlimited && !hasLimit {
			t.Errorf("Did not expect unlimited for labels %v", tc.userLabels)
			continue
		}
		if maxProjects != tc.expectedLimit {
			t.Errorf("Did not get expected limit for labels %v. Got: %d. Expected: %d", tc.userLabels, maxProjects, tc.expectedLimit)
		}
	}
}

func TestAdmit(t *testing.T) {
	tests := []struct {
		config          *ProjectRequestLimitConfig
		user            string
		expectForbidden bool
	}{
		{
			config: multiLevelConfig(),
			user:   "user1",
		},
		{
			config:          multiLevelConfig(),
			user:            "user2",
			expectForbidden: true,
		},
		{
			config: multiLevelConfig(),
			user:   "user3",
		},
		{
			config:          multiLevelConfig(),
			user:            "user4",
			expectForbidden: true,
		},
		{
			config: emptyConfig(),
			user:   "user2",
		},
		{
			config:          singleDefaultConfig(),
			user:            "user3",
			expectForbidden: true,
		},
		{
			config: singleDefaultConfig(),
			user:   "user1",
		},
	}

	for _, tc := range tests {
		pCache := fakeProjectCache(map[string]int{
			"user2": 2,
			"user3": 5,
			"user4": 1,
		})
		client := &testclient.Fake{}
		client.AddReactor("get", "users", userFn(map[string]labels.Set{
			"user2": {"bronze": "yes"},
			"user3": {"platinum": "yes"},
			"user4": {"unknown": "yes"},
		}))
		reqLimit, err := NewProjectRequestLimit(tc.config)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}
		reqLimit.(oadmission.WantsOpenshiftClient).SetOpenshiftClient(client)
		reqLimit.(oadmission.WantsProjectCache).SetProjectCache(pCache)
		if err = reqLimit.(oadmission.Validator).Validate(); err != nil {
			t.Fatalf("validation error: %v", err)
		}
		err = reqLimit.Admit(admission.NewAttributesRecord(
			&projectapi.ProjectRequest{},
			projectapi.Kind("ProjectRequest"),
			"foo",
			"name",
			projectapi.Resource("projectrequests"),
			"",
			"CREATE",
			&user.DefaultInfo{Name: tc.user}))
		if err != nil && !tc.expectForbidden {
			t.Errorf("Got unexpected error for user %s: %v", tc.user, err)
			continue
		}
		if !apierrors.IsForbidden(err) && tc.expectForbidden {
			t.Errorf("Expecting forbidden error for user %s and config %#v. Got: %v", tc.user, tc.config, err)
		}
	}
}

func intp(n int) *int {
	return &n
}

func selectorEquals(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func configEquals(a, b *ProjectRequestLimitConfig) bool {
	if len(a.Limits) != len(b.Limits) {
		return false
	}
	for n, limit := range a.Limits {
		limit2 := b.Limits[n]
		if !selectorEquals(limit.Selector, limit2.Selector) {
			return false
		}
		if (limit.MaxProjects == nil || limit2.MaxProjects == nil) && limit.MaxProjects != limit2.MaxProjects {
			return false
		}
		if limit.MaxProjects == nil {
			continue
		}
		if *limit.MaxProjects != *limit2.MaxProjects {
			return false
		}
	}
	return true
}

func fakeNs(name string) *kapi.Namespace {
	ns := &kapi.Namespace{}
	ns.Name = kapi.SimpleNameGenerator.GenerateName("testns")
	ns.Annotations = map[string]string{
		"openshift.io/requester": name,
	}
	return ns
}

func fakeUser(name string, labels map[string]string) *userapi.User {
	user := &userapi.User{}
	user.Name = name
	user.Labels = labels
	return user
}

func fakeProjectCache(requesters map[string]int) *projectcache.ProjectCache {
	kclient := &ktestclient.Fake{}
	pCache := projectcache.NewFake(kclient.Namespaces(), projectcache.NewCacheStore(cache.MetaNamespaceKeyFunc), "")
	for requester, count := range requesters {
		for i := 0; i < count; i++ {
			pCache.Store.Add(fakeNs(requester))
		}
	}
	return pCache
}

func userFn(usersAndLabels map[string]labels.Set) ktestclient.ReactionFunc {
	return func(action ktestclient.Action) (handled bool, ret runtime.Object, err error) {
		name := action.(ktestclient.GetAction).GetName()
		return true, fakeUser(name, map[string]string(usersAndLabels[name])), nil
	}
}

func multiLevelConfig() *ProjectRequestLimitConfig {
	return &ProjectRequestLimitConfig{
		Limits: []ProjectLimitBySelector{
			{
				Selector:    map[string]string{"platinum": "yes"},
				MaxProjects: nil,
			},
			{
				Selector:    map[string]string{"gold": "yes"},
				MaxProjects: intp(10),
			},
			{
				Selector:    map[string]string{"silver": "yes"},
				MaxProjects: intp(3),
			},
			{
				Selector:    map[string]string{"bronze": "yes"},
				MaxProjects: intp(2),
			},
			{
				Selector:    map[string]string{},
				MaxProjects: intp(1),
			},
		},
	}
}

func emptyConfig() *ProjectRequestLimitConfig {
	return &ProjectRequestLimitConfig{}
}

func singleDefaultConfig() *ProjectRequestLimitConfig {
	return &ProjectRequestLimitConfig{
		Limits: []ProjectLimitBySelector{
			{
				Selector:    nil,
				MaxProjects: intp(1),
			},
		},
	}
}
