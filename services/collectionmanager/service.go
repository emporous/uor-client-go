package collectionmanager

import (
	"context"
	"os"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"oras.land/oras-go/v2/content/file"

	"github.com/uor-framework/uor-client-go/api/client/v1alpha1"
	managerapi "github.com/uor-framework/uor-client-go/api/services/collectionmanager/v1alpha1"
	"github.com/uor-framework/uor-client-go/attributes/matchers"
	"github.com/uor-framework/uor-client-go/config"
	"github.com/uor-framework/uor-client-go/content"
	"github.com/uor-framework/uor-client-go/log"
	"github.com/uor-framework/uor-client-go/manager"
	"github.com/uor-framework/uor-client-go/registryclient"
	"github.com/uor-framework/uor-client-go/registryclient/orasclient"
	"github.com/uor-framework/uor-client-go/util/workspace"
)

var _ managerapi.CollectionManagerServer = &service{}

type service struct {
	managerapi.UnimplementedCollectionManagerServer
	mg      manager.Manager
	options ServiceOptions
}

// ServiceOptions configure the collection manager service with default remote
// and collection caching options.
type ServiceOptions struct {
	Insecure       bool
	PlainHTTP      bool
	PullCache      content.Store
	Logger         log.Logger
	RegistryConfig registryclient.RegistryConfig
}

// FromManager returns a CollectionManager API server from a Manager type.
func FromManager(mg manager.Manager, serviceOptions ServiceOptions) (managerapi.CollectionManagerServer, error) {
	if serviceOptions.Logger == nil {
		logger, err := log.NewLogrusLogger(os.Stderr, "debug")
		if err != nil {
			return nil, err
		}
		serviceOptions.Logger = logger
	}
	return &service{
		mg:      mg,
		options: serviceOptions,
	}, nil
}

// PublishContent publishes collection content to a storage provide based on client input.
func (s *service) PublishContent(ctx context.Context, message *managerapi.Publish_Request) (*managerapi.Publish_Response, error) {
	authConf := authConfig{message.Auth}
	client, err := orasclient.NewClient(
		orasclient.WithCache(s.options.PullCache),
		orasclient.WithPlainHTTP(s.options.PlainHTTP),
		orasclient.WithCredentialFunc(authConf.Credential),
		orasclient.SkipTLSVerify(s.options.Insecure),
		orasclient.WithRegistryConfig(s.options.RegistryConfig),
	)
	if err != nil {
		return &managerapi.Publish_Response{}, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err := client.Destroy(); err != nil {
			s.options.Logger.Errorf(err.Error())
		}
	}()

	space, err := workspace.NewLocalWorkspace(message.Source)
	if err != nil {
		return &managerapi.Publish_Response{}, status.Error(codes.Internal, err.Error())
	}

	var dsConfig v1alpha1.DataSetConfiguration
	if message.Collection != nil {
		var files []v1alpha1.File
		for _, file := range message.Collection.Files {
			f := v1alpha1.File{
				File:       file.File,
				Attributes: file.Attributes.AsMap(),
			}
			files = append(files, f)
		}

		dsConfig = v1alpha1.DataSetConfiguration{
			TypeMeta: v1alpha1.TypeMeta{
				Kind:       v1alpha1.DataSetConfigurationKind,
				APIVersion: v1alpha1.GroupVersion,
			},
			Collection: v1alpha1.DataSetConfigurationSpec{
				SchemaAddress:     message.Collection.SchemaAddress,
				LinkedCollections: message.Collection.LinkedCollections,
				Files:             files,
			},
		}
	}

	_, err = s.mg.Build(ctx, space, dsConfig, message.Destination, client)
	if err != nil {
		if err != nil {
			return &managerapi.Publish_Response{}, status.Error(codes.Internal, err.Error())
		}
	}

	digest, err := s.mg.Push(ctx, message.Destination, client)
	if err != nil {
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	return &managerapi.Publish_Response{Digest: digest}, nil
}

// RetrieveContent retrieves collection contact from a storage provider based on client input.
func (s *service) RetrieveContent(ctx context.Context, message *managerapi.Retrieve_Request) (*managerapi.Retrieve_Response, error) {
	attrSet, err := config.ConvertToModel(message.Filter.AsMap())
	if err != nil {
		return &managerapi.Retrieve_Response{}, status.Error(codes.Internal, err.Error())
	}

	authConf := authConfig{message.Auth}
	var matcher matchers.PartialAttributeMatcher = attrSet.List()
	client, err := orasclient.NewClient(
		orasclient.WithCache(s.options.PullCache),
		orasclient.WithCredentialFunc(authConf.Credential),
		orasclient.WithPlainHTTP(s.options.PlainHTTP),
		orasclient.SkipTLSVerify(s.options.Insecure),
		orasclient.WithPullableAttributes(matcher),
		orasclient.WithRegistryConfig(s.options.RegistryConfig),
	)
	if err != nil {
		return &managerapi.Retrieve_Response{}, status.Error(codes.Internal, err.Error())
	}
	defer func() {
		if err := client.Destroy(); err != nil {
			s.options.Logger.Errorf(err.Error())
		}
	}()

	digests, err := s.mg.PullAll(ctx, message.Source, client, file.New(message.Destination))
	if err != nil {
		return &managerapi.Retrieve_Response{}, status.Error(codes.Internal, err.Error())
	}

	if len(digests) == 0 {
		return &managerapi.Retrieve_Response{
			Digests: nil,
			Diagnostics: []*managerapi.Diagnostic{
				{
					Severity: 2,
					Summary:  "RetrieveWarning",
					Detail:   "No matching collections found",
				},
			},
		}, nil
	}

	return &managerapi.Retrieve_Response{Digests: digests}, nil
}
