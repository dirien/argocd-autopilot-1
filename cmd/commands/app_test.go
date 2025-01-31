package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"testing"

	"github.com/argoproj/argocd-autopilot/pkg/application"
	appmocks "github.com/argoproj/argocd-autopilot/pkg/application/mocks"
	"github.com/argoproj/argocd-autopilot/pkg/fs"
	fsmocks "github.com/argoproj/argocd-autopilot/pkg/fs/mocks"
	"github.com/argoproj/argocd-autopilot/pkg/git"
	gitmocks "github.com/argoproj/argocd-autopilot/pkg/git/mocks"
	"github.com/argoproj/argocd-autopilot/pkg/kube"
	"github.com/argoproj/argocd-autopilot/pkg/store"

	"github.com/ghodss/yaml"
	"github.com/go-git/go-billy/v5/memfs"
	osfs "github.com/go-git/go-billy/v5/osfs"
	billyUtils "github.com/go-git/go-billy/v5/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	kusttypes "sigs.k8s.io/kustomize/api/types"
)

func Test_getCommitMsg(t *testing.T) {
	tests := map[string]struct {
		appName     string
		projectName string
		root        string
		expected    string
	}{
		"On root": {
			appName:     "foo",
			projectName: "bar",
			root:        "",
			expected:    "installed app 'foo' on project 'bar'",
		},
		"On installation path": {
			appName:     "foo",
			projectName: "bar",
			root:        "foo/bar",
			expected:    "installed app 'foo' on project 'bar' installation-path: 'foo/bar'",
		},
	}
	for tname, tt := range tests {
		t.Run(tname, func(t *testing.T) {
			m := &fsmocks.FS{}
			m.On("Root").Return(tt.root)
			opts := &AppCreateOptions{
				BaseOptions: BaseOptions{
					ProjectName: tt.projectName,
				},
				AppOpts: &application.CreateOptions{
					AppName: tt.appName,
				},
			}
			got := getCommitMsg(opts, m)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func Test_writeApplicationFile(t *testing.T) {
	type args struct {
		root string
		path string
		name string
		data []byte
	}
	tests := map[string]struct {
		args     args
		assertFn func(t *testing.T, repofs fs.FS, exists bool, err error)
		beforeFn func(repofs fs.FS) fs.FS
	}{
		"On Root": {
			args: args{
				path: "foo/bar",
				name: "test",
				data: []byte("data"),
			},
			assertFn: func(t *testing.T, repofs fs.FS, exists bool, ret error) {
				assert.NoError(t, ret)

				f, err := repofs.Open("/foo/bar")
				assert.NoError(t, err)
				d, err := ioutil.ReadAll(f)
				assert.NoError(t, err)

				assert.Equal(t, d, []byte("data"))
				assert.False(t, exists)
			},
		},
		"With Chroot": {
			args: args{
				root: "someroot",
				path: "foo/bar",
				name: "test",
				data: []byte("data2"),
			},
			assertFn: func(t *testing.T, repofs fs.FS, exists bool, ret error) {
				assert.NoError(t, ret)

				assert.Equal(t, "/someroot", repofs.Root())
				f, err := repofs.Open("/foo/bar")
				assert.NoError(t, err)
				d, err := ioutil.ReadAll(f)
				assert.NoError(t, err)

				assert.Equal(t, d, []byte("data2"))
				assert.False(t, exists)
			},
		},
		"File exists": {
			args: args{
				path: "foo/bar",
				name: "test",
				data: []byte("data2"),
			},
			beforeFn: func(repofs fs.FS) fs.FS {
				_ = billyUtils.WriteFile(repofs, "/foo/bar", []byte("data"), 0666)
				return repofs
			},
			assertFn: func(t *testing.T, _ fs.FS, exists bool, ret error) {
				assert.NoError(t, ret)
				assert.True(t, exists)
			},
		},
		"Write error": {
			args: args{
				path: "foo/bar",
				name: "test",
				data: []byte("data2"),
			},
			beforeFn: func(_ fs.FS) fs.FS {
				mfs := &fsmocks.FS{}
				mfs.On("CheckExistsOrWrite", mock.Anything, mock.Anything).Return(false, fmt.Errorf("error"))
				mfs.On("Root").Return("/")
				mfs.On("Join", mock.Anything, mock.Anything).Return("/foo/bar")
				return mfs
			},
			assertFn: func(t *testing.T, _ fs.FS, _ bool, ret error) {
				assert.Error(t, ret)
				assert.EqualError(t, ret, "failed to create 'test' file at '/foo/bar': error")
			},
		},
	}
	for tname, tt := range tests {
		t.Run(tname, func(t *testing.T) {
			repofs := fs.Create(memfs.New())
			if tt.args.root != "" {
				bfs, _ := repofs.Chroot(tt.args.root)
				repofs = fs.Create(bfs)
			}

			if tt.beforeFn != nil {
				repofs = tt.beforeFn(repofs)
			}

			got, err := writeApplicationFile(repofs, tt.args.path, tt.args.name, tt.args.data)
			tt.assertFn(t, repofs, got, err)
		})
	}
}

func Test_createApplicationFiles(t *testing.T) {
	getAppMock := func() *appmocks.Application {
		app := &appmocks.Application{}
		app.On("Name").Return("foo")
		app.On("Config").Return(&application.Config{})
		app.On("Namespace").Return(kube.GenerateNamespace("foo"))
		app.On("Manifests").Return(nil)
		app.On("Base").Return(&kusttypes.Kustomization{
			TypeMeta: kusttypes.TypeMeta{
				APIVersion: kusttypes.KustomizationVersion,
				Kind:       kusttypes.KustomizationKind,
			},
			Resources: []string{"foo"},
		})
		app.On("Overlay").Return(&kusttypes.Kustomization{
			TypeMeta: kusttypes.TypeMeta{
				APIVersion: kusttypes.KustomizationVersion,
				Kind:       kusttypes.KustomizationKind,
			},
			Resources: []string{"foo"},
		})
		return app
	}

	tests := map[string]struct {
		projectName string
		beforeFn    func(*testing.T) (fs.FS, application.Application, string)
		assertFn    func(*testing.T, fs.FS, application.Application, error)
	}{
		"New application": {
			beforeFn: func(t *testing.T) (fs.FS, application.Application, string) {
				app := getAppMock()
				root, err := ioutil.TempDir("", "test")
				assert.NoError(t, err)
				repofs := fs.Create(osfs.New(root))

				return repofs, app, "fooproj"
			},
			assertFn: func(t *testing.T, repofs fs.FS, _ application.Application, ret error) {
				defer os.RemoveAll(repofs.Root()) // remove temp dir
				assert.NoError(t, ret)
				assert.DirExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir), "kustomization dir should exist")
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "base", "kustomization.yaml"))
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "kustomization.yaml"))
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "namespace.yaml"))
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "config.json"))
			},
		},
		"Application with flat installation no namespace": {
			beforeFn: func(t *testing.T) (fs.FS, application.Application, string) {
				app := &appmocks.Application{}
				app.On("Name").Return("foo")
				app.On("Config").Return(&application.Config{})
				app.On("Namespace").Return(nil)
				app.On("Manifests").Return([]byte(""))
				app.On("Base").Return(&kusttypes.Kustomization{
					TypeMeta: kusttypes.TypeMeta{
						APIVersion: kusttypes.KustomizationVersion,
						Kind:       kusttypes.KustomizationKind,
					},
					Resources: []string{"foo"},
				})
				app.On("Overlay").Return(&kusttypes.Kustomization{
					TypeMeta: kusttypes.TypeMeta{
						APIVersion: kusttypes.KustomizationVersion,
						Kind:       kusttypes.KustomizationKind,
					},
					Resources: []string{"foo"},
				})

				root, err := ioutil.TempDir("", "test")
				assert.NoError(t, err)
				repofs := fs.Create(osfs.New(root))

				return repofs, app, "fooproj"
			},
			assertFn: func(t *testing.T, repofs fs.FS, _ application.Application, ret error) {
				defer os.RemoveAll(repofs.Root()) // remove temp dir
				assert.NoError(t, ret)
				assert.DirExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir), "kustomization dir should exist")
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "base", "kustomization.yaml"))
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "base", "install.yaml"))                    // installation manifests
				assert.NoFileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "namespace.yaml")) // no namespace
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "kustomization.yaml"))
				assert.FileExists(t, repofs.Join(repofs.Root(), store.Default.KustomizeDir, "foo", "overlays", "fooproj", "config.json"))
			},
		},
		"App base collision": {
			beforeFn: func(t *testing.T) (fs.FS, application.Application, string) {
				app := getAppMock()
				root, err := ioutil.TempDir("", "test")
				assert.NoError(t, err)
				repofs := fs.Create(osfs.New(root))
				// change the original base to make collision with the new one
				orgBase := &kusttypes.Kustomization{
					TypeMeta: kusttypes.TypeMeta{
						APIVersion: kusttypes.KustomizationVersion,
						Kind:       kusttypes.KustomizationKind,
					},
					Resources: []string{"bar"}, // different resources
				}
				orgBaseYAML, err := yaml.Marshal(orgBase)
				assert.NoError(t, err)
				err = billyUtils.WriteFile(repofs, repofs.Join(store.Default.KustomizeDir, "foo", "base", "kustomization.yaml"), orgBaseYAML, 0666)
				assert.NoError(t, err)
				err = billyUtils.WriteFile(repofs, repofs.Join(store.Default.KustomizeDir, "foo", "overlays", "fooproj", "kustomization.yaml"), []byte(""), 0666)
				assert.NoError(t, err)

				return repofs, app, "fooproj"
			},
			assertFn: func(t *testing.T, repofs fs.FS, _ application.Application, ret error) {
				defer os.RemoveAll(repofs.Root()) // remove temp dir
				assert.ErrorIs(t, ret, ErrAppCollisionWithExistingBase)
			},
		},
		"Same app base overlay already exists": {
			beforeFn: func(t *testing.T) (fs.FS, application.Application, string) {
				app := getAppMock()
				root, err := ioutil.TempDir("", "test")
				assert.NoError(t, err)
				repofs := fs.Create(osfs.New(root))
				sameBase, err := yaml.Marshal(app.Base())
				assert.NoError(t, err)
				err = billyUtils.WriteFile(repofs, repofs.Join(store.Default.KustomizeDir, "foo", "base", "kustomization.yaml"), sameBase, 0666)
				assert.NoError(t, err)
				err = billyUtils.WriteFile(repofs, repofs.Join(store.Default.KustomizeDir, "foo", "overlays", "fooproj", "kustomization.yaml"), []byte(""), 0666)
				assert.NoError(t, err)

				return repofs, app, "fooproj"
			},
			assertFn: func(t *testing.T, repofs fs.FS, _ application.Application, ret error) {
				defer os.RemoveAll(repofs.Root()) // remove temp dir
				assert.ErrorIs(t, ret, ErrAppAlreadyInstalledOnProject)
			},
		},
	}
	for tname, tt := range tests {
		t.Run(tname, func(t *testing.T) {
			repofs, app, proj := tt.beforeFn(t)
			err := createApplicationFiles(repofs, app, proj)
			tt.assertFn(t, repofs, app, err)
		})
	}
}

func Test_getConfigFileFromPath(t *testing.T) {
	tests := map[string]struct {
		appName  string
		want     *application.Config
		wantErr  string
		beforeFn func(repofs fs.FS, appName string) fs.FS
		assertFn func(t *testing.T, conf *application.Config)
	}{
		"should return config.json": {
			want: &application.Config{
				AppName: "test",
			},
			appName: "test",
			beforeFn: func(repofs fs.FS, appName string) fs.FS {
				conf := application.Config{AppName: appName}
				b, _ := json.Marshal(&conf)
				_ = billyUtils.WriteFile(repofs, fmt.Sprintf("%s/config.json", appName), b, 0666)
				return repofs
			},
			assertFn: func(t *testing.T, conf *application.Config) {
				assert.Equal(t, conf.AppName, "test")
			},
		},
		"should fail if config.json is missing": {
			appName:  "test",
			want:     &application.Config{},
			wantErr:  "test/config.json not found",
			beforeFn: nil,
			assertFn: nil,
		},
		"should fail if config.json failed to unmarshal": {
			appName: "test",
			want:    &application.Config{},
			wantErr: "failed to unmarshal file test/config.json",
			beforeFn: func(repofs fs.FS, appName string) fs.FS {
				_ = billyUtils.WriteFile(repofs, fmt.Sprintf("%s/config.json", appName), []byte{}, 0666)
				return repofs
			},
			assertFn: nil,
		},
	}
	for tname, tt := range tests {
		t.Run(tname, func(t *testing.T) {
			repofs := fs.Create(memfs.New())
			if tt.beforeFn != nil {
				repofs = tt.beforeFn(repofs, tt.appName)
			}

			got, err := getConfigFileFromPath(repofs, tt.appName)
			if err != nil && tt.wantErr != "" {
				assert.EqualError(t, err, tt.wantErr)
			}

			if (err != nil) && tt.wantErr == "" {
				t.Errorf("getConfigFileFromPath() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.assertFn != nil {
				tt.assertFn(t, got)
			}
		})
	}
}

func TestRunAppDelete(t *testing.T) {
	tests := map[string]struct {
		appName     string
		projectName string
		global      bool
		wantErr     string
		prepareRepo func() (git.Repository, fs.FS, error)
		assertFn    func(t *testing.T, repo git.Repository, repofs fs.FS)
	}{
		"Should fail when clone fails": {
			wantErr: "some error",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				return nil, nil, fmt.Errorf("some error")
			},
		},
		"Should fail when app does not exist": {
			appName: "app",
			wantErr: "application 'app' not found",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				return nil, fs.Create(memfs.New()), nil
			},
		},
		"Should fail if deletion of entire app directory fails": {
			appName: "app",
			global:  true,
			wantErr: "failed to delete directory 'kustomize/app': some error",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				mfs := &fsmocks.FS{}
				mfs.On("Join", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(func(elem ...string) string {
					return strings.Join(elem, "/")
				})
				mfs.On("ExistsOrDie", "kustomize/app").Return(true)
				mfs.On("Remove", "kustomize/app").Return(fmt.Errorf("some error"))
				mfs.On("Stat", "kustomize/app").Return(nil, fmt.Errorf("some error"))
				return nil, mfs, nil
			},
		},
		"Should remove entire app directory when global flag is set": {
			appName: "app",
			global:  true,
			prepareRepo: func() (git.Repository, fs.FS, error) {
				memfs := memfs.New()
				_ = memfs.MkdirAll("kustomize/app/overlays/project", 0666)
				mockRepo := &gitmocks.Repository{}
				mockRepo.On("Persist", mock.AnythingOfType("*context.emptyCtx"), &git.PushOptions{
					CommitMsg: "Deleted app 'app'",
				}).Return(nil)
				return mockRepo, fs.Create(memfs), nil
			},
			assertFn: func(t *testing.T, repo git.Repository, repofs fs.FS) {
				repo.(*gitmocks.Repository).AssertExpectations(t)
				assert.False(t, repofs.ExistsOrDie("kustomize/app"))
			},
		},
		"Should fail when overlay does not exist": {
			appName:     "app",
			projectName: "project",
			wantErr:     "application 'app' not found in project 'project'",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				memfs := memfs.New()
				_ = memfs.MkdirAll("kustomize/app/overlays/project2", 0666)
				return nil, fs.Create(memfs), nil
			},
		},
		"Should fail if ReadDir fails": {
			appName:     "app",
			projectName: "project",
			wantErr:     "failed to read overlays directory 'kustomize/app/overlays': some error",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				mfs := &fsmocks.FS{}
				mfs.On("Join", mock.AnythingOfType("string"), mock.AnythingOfType("string")).Return(func(elem ...string) string {
					return strings.Join(elem, "/")
				})
				mfs.On("ExistsOrDie", "kustomize/app").Return(true)
				mfs.On("ExistsOrDie", "kustomize/app/overlays/project").Return(true)
				mfs.On("ReadDir", "kustomize/app/overlays").Return(nil, fmt.Errorf("some error"))
				return nil, mfs, nil
			},
		},
		"Should delete only overlay directory, if there are more overlays": {
			appName:     "app",
			projectName: "project",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				memfs := memfs.New()
				_ = memfs.MkdirAll("kustomize/app/overlays/project", 0666)
				_ = memfs.MkdirAll("kustomize/app/overlays/project2", 0666)
				mockRepo := &gitmocks.Repository{}
				mockRepo.On("Persist", mock.AnythingOfType("*context.emptyCtx"), &git.PushOptions{
					CommitMsg: "Deleted app 'app' from project 'project'",
				}).Return(nil)
				return mockRepo, fs.Create(memfs), nil
			},
			assertFn: func(t *testing.T, repo git.Repository, repofs fs.FS) {
				repo.(*gitmocks.Repository).AssertExpectations(t)
				assert.True(t, repofs.ExistsOrDie("kustomize/app/overlays"))
				assert.False(t, repofs.ExistsOrDie("kustomize/app/overlays/project"))
			},
		},
		"Should delete entire app directory, if there are no more overlays": {
			appName:     "app",
			projectName: "project",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				memfs := memfs.New()
				_ = memfs.MkdirAll("kustomize/app/overlays/project", 0666)
				mockRepo := &gitmocks.Repository{}
				mockRepo.On("Persist", mock.AnythingOfType("*context.emptyCtx"), &git.PushOptions{
					CommitMsg: "Deleted app 'app'",
				}).Return(nil)
				return mockRepo, fs.Create(memfs), nil
			},
			assertFn: func(t *testing.T, repo git.Repository, repofs fs.FS) {
				repo.(*gitmocks.Repository).AssertExpectations(t)
				assert.False(t, repofs.ExistsOrDie("kustomize/app"))
			},
		},
		"Should fail if Persist fails": {
			appName: "app",
			global:  true,
			wantErr: "failed to push to repo: some error",
			prepareRepo: func() (git.Repository, fs.FS, error) {
				memfs := memfs.New()
				_ = memfs.MkdirAll("kustomize/app/overlays/project", 0666)
				mockRepo := &gitmocks.Repository{}
				mockRepo.On("Persist", mock.AnythingOfType("*context.emptyCtx"), &git.PushOptions{
					CommitMsg: "Deleted app 'app'",
				}).Return(fmt.Errorf("some error"))
				return mockRepo, fs.Create(memfs), nil
			},
			assertFn: func(t *testing.T, repo git.Repository, repofs fs.FS) {
				repo.(*gitmocks.Repository).AssertExpectations(t)
				assert.False(t, repofs.ExistsOrDie("kustomize/app"))
			},
		},
	}
	origPrepareRepo := prepareRepo
	defer func() { prepareRepo = origPrepareRepo }()
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var (
				repo   git.Repository
				repofs fs.FS
			)

			prepareRepo = func(_ context.Context, _ *BaseOptions) (git.Repository, fs.FS, error) {
				var err error
				repo, repofs, err = tt.prepareRepo()
				return repo, repofs, err
			}
			opts := &AppDeleteOptions{
				BaseOptions: BaseOptions{
					ProjectName: tt.projectName,
					FS:          fs.Create(memfs.New()),
				},
				AppName: tt.appName,
				Global:  tt.global,
			}
			if err := RunAppDelete(context.Background(), opts); err != nil {
				if tt.wantErr != "" {
					assert.EqualError(t, err, tt.wantErr)
				} else {
					t.Errorf("prepare() error = %v", err)
				}

				return
			}

			if tt.assertFn != nil {
				tt.assertFn(t, repo, repofs)
			}
		})
	}
}
