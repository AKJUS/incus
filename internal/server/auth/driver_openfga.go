package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"time"

	openfga "github.com/openfga/go-sdk"
	"github.com/openfga/go-sdk/client"
	"github.com/openfga/go-sdk/credentials"

	"github.com/lxc/incus/v6/internal/server/certificate"
	"github.com/lxc/incus/v6/shared/api"
	"github.com/lxc/incus/v6/shared/logger"
)

// FGA represents an OpenFGA authorizer.
type FGA struct {
	commonAuthorizer
	tls *TLS

	apiURL   string
	apiToken string
	storeID  string

	onlineMu sync.Mutex
	online   bool

	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	client *client.OpenFgaClient
}

func (f *FGA) configure(opts Opts) error {
	if opts.config == nil {
		return errors.New("Missing OpenFGA config")
	}

	val, ok := opts.config["openfga.api.token"]
	if !ok || val == nil {
		return errors.New("Missing OpenFGA API token")
	}

	f.apiToken, ok = val.(string)
	if !ok {
		return fmt.Errorf("Expected a string for configuration key %q, got: %T", "openfga.api.token", val)
	}

	val, ok = opts.config["openfga.api.url"]
	if !ok || val == nil {
		return errors.New("Missing OpenFGA API URL")
	}

	f.apiURL, ok = val.(string)
	if !ok {
		return fmt.Errorf("Expected a string for configuration key %q, got: %T", "openfga.api.url", val)
	}

	val, ok = opts.config["openfga.store.id"]
	if !ok || val == nil {
		return errors.New("Missing OpenFGA store ID")
	}

	f.storeID, ok = val.(string)
	if !ok {
		return fmt.Errorf("Expected a string for configuration key %q, got: %T", "openfga.store.id", val)
	}

	return nil
}

func (f *FGA) load(ctx context.Context, certificateCache *certificate.Cache, opts Opts) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := f.configure(opts)
	if err != nil {
		return err
	}

	f.tls = &TLS{}
	err = f.tls.load(ctx, certificateCache, opts)
	if err != nil {
		return err
	}

	conf := client.ClientConfiguration{
		ApiUrl:  f.apiURL,
		StoreId: f.storeID,
		Credentials: &credentials.Credentials{
			Method: credentials.CredentialsMethodApiToken,
			Config: &credentials.Config{
				ApiToken: f.apiToken,
			},
		},
	}

	f.client, err = client.NewSdkClient(&conf)
	if err != nil {
		return fmt.Errorf("Failed to create OpenFGA client: %w", err)
	}

	f.shutdownCtx, f.shutdownCancel = context.WithCancel(context.Background())

	// Connect in the background.
	go func(ctx context.Context, certificateCache *certificate.Cache, opts Opts) {
		first := true

		for {
			// Attempt a connection.
			err := f.connect(ctx, certificateCache, opts)
			if err == nil {
				if !first {
					logger.Warn("Connection with OpenFGA established")
				}

				f.onlineMu.Lock()
				defer f.onlineMu.Unlock()
				f.online = true

				return
			}

			// Handle re-tries.
			if first {
				logger.Warn("Unable to connect to the OpenFGA server, will retry every 30s", logger.Ctx{"err": err})
				first = false
			}

			select {
			case <-time.After(30 * time.Second):
				continue
			case <-f.shutdownCtx.Done():
				return
			}
		}
	}(f.shutdownCtx, certificateCache, opts)

	return nil
}

// StopService stops the authorizer gracefully.
func (f *FGA) StopService(ctx context.Context) error {
	// Cancel any background routine.
	f.shutdownCancel()

	return nil
}

// ApplyPatch is called when an applicable server patch is run, this triggers a model re-upload.
func (f *FGA) ApplyPatch(ctx context.Context, name string) error {
	// Always refresh the model.
	logger.Info("Refreshing the OpenFGA model")
	err := f.refreshModel(ctx)
	if err != nil {
		return err
	}

	if name == "auth_openfga_viewer" {
		// Add the public access permission if not set.
		resp, err := f.client.Check(ctx).Body(client.ClientCheckRequest{
			User:     "user:*",
			Relation: "authenticated",
			Object:   ObjectServer().String(),
		}).Execute()
		if err != nil {
			return err
		}

		if !resp.GetAllowed() {
			err = f.sendTuples(ctx, []client.ClientTupleKey{
				{User: "user:*", Relation: "authenticated", Object: ObjectServer().String()},
			}, nil)
			if err != nil {
				return err
			}

			// Attempt to clear the former version of this permission.
			_ = f.sendTuples(ctx, nil, []client.ClientTupleKeyWithoutCondition{
				{User: "user:*", Relation: "viewer", Object: ObjectServer().String()},
			})
		}
	}

	return nil
}

func (f *FGA) refreshModel(ctx context.Context) error {
	var builtinAuthorizationModel client.ClientWriteAuthorizationModelRequest
	err := json.Unmarshal([]byte(authModel), &builtinAuthorizationModel)
	if err != nil {
		return fmt.Errorf("Failed to unmarshal built in authorization model: %w", err)
	}

	_, err = f.client.WriteAuthorizationModel(ctx).Body(builtinAuthorizationModel).Execute()
	if err != nil {
		return fmt.Errorf("Failed to write the authorization model: %w", err)
	}

	return nil
}

func (f *FGA) connect(ctx context.Context, certificateCache *certificate.Cache, opts Opts) error {
	// Load current authorization model.
	readModelResponse, err := f.client.ReadLatestAuthorizationModel(ctx).Execute()
	if err != nil {
		return fmt.Errorf("Failed to read pre-existing OpenFGA model: %w", err)
	}

	// Check if we need to upload an initial model.
	if readModelResponse.AuthorizationModel == nil {
		logger.Info("Upload initial OpenFGA model")

		// Upload the model itself.
		err := f.refreshModel(ctx)
		if err != nil {
			return fmt.Errorf("Failed to load initial model: %w", err)
		}

		// Allow basic authenticated access.
		err = f.sendTuples(ctx, []client.ClientTupleKey{
			{User: "user:*", Relation: "authenticated", Object: ObjectServer().String()},
		}, nil)
		if err != nil {
			return err
		}
	}

	if opts.resourcesFunc != nil {
		// Start resource sync routine.
		go func(resourcesFunc func() (*Resources, error)) {
			for {
				resources, err := resourcesFunc()
				if err == nil {
					// resources will be nil on cluster members that shouldn't be performing updates.
					if resources != nil {
						err := f.syncResources(f.shutdownCtx, *resources)
						if err != nil {
							logger.Error("Failed background OpenFGA resource sync", logger.Ctx{"err": err})
						}
					}
				} else {
					logger.Error("Failed getting local OpenFGA resources", logger.Ctx{"err": err})
				}

				select {
				case <-time.After(time.Hour):
					continue
				case <-f.shutdownCtx.Done():
					return
				}
			}
		}(opts.resourcesFunc)
	}

	return nil
}

// CheckPermission returns an error if the user does not have the given Entitlement on the given Object.
func (f *FGA) CheckPermission(ctx context.Context, r *http.Request, object Object, entitlement Entitlement) error {
	logCtx := logger.Ctx{"object": object, "entitlement": entitlement, "url": r.URL.String(), "method": r.Method}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	details, err := f.requestDetails(r)
	if err != nil {
		return api.StatusErrorf(http.StatusForbidden, "Failed to extract request details: %v", err)
	}

	if details.isInternalOrUnix() {
		return nil
	}

	// Use the TLS driver if the user authenticated with TLS.
	if details.authenticationProtocol() == api.AuthenticationMethodTLS {
		return f.tls.CheckPermission(ctx, r, object, entitlement)
	}

	// If offline, return a clear error to the user.
	f.onlineMu.Lock()
	defer f.onlineMu.Unlock()
	if !f.online {
		return api.StatusErrorf(http.StatusForbidden, "The authorization server is currently offline, please try again later")
	}

	username := details.username()
	logCtx["username"] = username
	logCtx["protocol"] = details.Protocol

	objectUser := ObjectUser(username)
	body := client.ClientCheckRequest{
		User:     objectUser.String(),
		Relation: string(entitlement),
		Object:   object.String(),
	}

	f.logger.Debug("Checking OpenFGA relation", logCtx)
	resp, err := f.client.Check(ctx).Body(body).Execute()
	if err != nil {
		return fmt.Errorf("Failed to check OpenFGA relation: %w", err)
	}

	if !resp.GetAllowed() {
		return api.StatusErrorf(http.StatusForbidden, "User does not have entitlement %q on object %q", entitlement, object)
	}

	return nil
}

// GetPermissionChecker returns a function that can be used to check whether a user has the required entitlement on an authorization object.
func (f *FGA) GetPermissionChecker(ctx context.Context, r *http.Request, entitlement Entitlement, objectType ObjectType) (PermissionChecker, error) {
	allowFunc := func(b bool) func(Object) bool {
		return func(Object) bool {
			return b
		}
	}

	logCtx := logger.Ctx{"object_type": objectType, "entitlement": entitlement, "url": r.URL.String(), "method": r.Method}
	details, err := f.requestDetails(r)
	if err != nil {
		return nil, api.StatusErrorf(http.StatusForbidden, "Failed to extract request details: %v", err)
	}

	if details.isInternalOrUnix() {
		return allowFunc(true), nil
	}

	// Use the TLS driver if the user authenticated with TLS.
	if details.authenticationProtocol() == api.AuthenticationMethodTLS {
		return f.tls.GetPermissionChecker(ctx, r, entitlement, objectType)
	}

	username := details.username()
	logCtx["username"] = username
	logCtx["protocol"] = details.Protocol

	f.logger.Debug("Listing related objects for user", logCtx)
	resp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
		User:     ObjectUser(username).String(),
		Relation: string(entitlement),
		Type:     string(objectType),
	}).Execute()
	if err != nil {
		return nil, fmt.Errorf("Failed to OpenFGA objects of type %q with relation %q for user %q: %w", objectType, entitlement, username, err)
	}

	objects := resp.GetObjects()

	return func(object Object) bool {
		return slices.Contains(objects, object.String())
	}, nil
}

// AddProject adds a project to the authorizer.
func (f *FGA) AddProject(ctx context.Context, _ int64, projectName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectProject(projectName).String(),
		},
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, "default").String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteProject deletes a project from the authorizer.
func (f *FGA) DeleteProject(ctx context.Context, _ int64, projectName string) error {
	// Only empty projects can be deleted, so we don't need to worry about any tuples with this project as a parent.
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			// Remove the default profile
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, "default").String(),
		},
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectProject(projectName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameProject renames a project in the authorizer.
func (f *FGA) RenameProject(ctx context.Context, _ int64, oldName string, newName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectProject(newName).String(),
		},
		{
			User:     ObjectProject(newName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(newName, "default").String(),
		},
	}

	// Only empty projects can be renamed, so we don't need to worry about any tuples with this project as a parent.
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			// Remove the default profile
			User:     ObjectProject(oldName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(oldName, "default").String(),
		},
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectProject(oldName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddCertificate adds a certificate to the authorizer.
func (f *FGA) AddCertificate(ctx context.Context, fingerprint string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectCertificate(fingerprint).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteCertificate deletes a certificate from the authorizer.
func (f *FGA) DeleteCertificate(ctx context.Context, fingerprint string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectCertificate(fingerprint).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// AddStoragePool adds a storage pool to the authorizer.
func (f *FGA) AddStoragePool(ctx context.Context, storagePoolName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectStoragePool(storagePoolName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteStoragePool deletes a storage pool from the authorizer.
func (f *FGA) DeleteStoragePool(ctx context.Context, storagePoolName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectStoragePool(storagePoolName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// AddImage adds an image to the authorizer.
func (f *FGA) AddImage(ctx context.Context, projectName string, fingerprint string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImage(projectName, fingerprint).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteImage deletes an image from the authorizer.
func (f *FGA) DeleteImage(ctx context.Context, projectName string, fingerprint string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImage(projectName, fingerprint).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// AddImageAlias adds an image alias to the authorizer.
func (f *FGA) AddImageAlias(ctx context.Context, projectName string, imageAliasName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImageAlias(projectName, imageAliasName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteImageAlias deletes an image alias from the authorizer.
func (f *FGA) DeleteImageAlias(ctx context.Context, projectName string, imageAliasName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImageAlias(projectName, imageAliasName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameImageAlias renames an image alias in the authorizer.
func (f *FGA) RenameImageAlias(ctx context.Context, projectName string, oldAliasName string, newAliasName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImageAlias(projectName, newAliasName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectImageAlias(projectName, oldAliasName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddInstance adds an instance to the authorizer.
func (f *FGA) AddInstance(ctx context.Context, projectName string, instanceName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectInstance(projectName, instanceName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteInstance deletes an instance from the authorizer.
func (f *FGA) DeleteInstance(ctx context.Context, projectName string, instanceName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectInstance(projectName, instanceName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameInstance renames an instance in the authorizer.
func (f *FGA) RenameInstance(ctx context.Context, projectName string, oldInstanceName string, newInstanceName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectInstance(projectName, newInstanceName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectInstance(projectName, oldInstanceName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddNetwork adds a network to the authorizer.
func (f *FGA) AddNetwork(ctx context.Context, projectName string, networkName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetwork(projectName, networkName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteNetwork deletes a network from the authorizer.
func (f *FGA) DeleteNetwork(ctx context.Context, projectName string, networkName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetwork(projectName, networkName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameNetwork renames a network in the authorizer.
func (f *FGA) RenameNetwork(ctx context.Context, projectName string, oldNetworkName string, newNetworkName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetwork(projectName, newNetworkName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetwork(projectName, oldNetworkName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddNetworkZone adds a network zone in the authorizer.
func (f *FGA) AddNetworkZone(ctx context.Context, projectName string, networkZoneName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkZone(projectName, networkZoneName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteNetworkZone deletes a network zone from the authorizer.
func (f *FGA) DeleteNetworkZone(ctx context.Context, projectName string, networkZoneName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkZone(projectName, networkZoneName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// AddNetworkIntegration adds a network integration to the authorizer.
func (f *FGA) AddNetworkIntegration(ctx context.Context, networkIntegrationName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectNetworkIntegration(networkIntegrationName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteNetworkIntegration deletes a network integration from the authorizer.
func (f *FGA) DeleteNetworkIntegration(ctx context.Context, networkIntegrationName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectNetworkIntegration(networkIntegrationName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameNetworkIntegration renames a network integration in the authorizer.
func (f *FGA) RenameNetworkIntegration(ctx context.Context, oldNetworkIntegrationName string, newNetworkIntegrationName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectNetworkIntegration(newNetworkIntegrationName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectServer().String(),
			Relation: relationServer,
			Object:   ObjectNetworkIntegration(oldNetworkIntegrationName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddNetworkACL adds a network ACL in the authorizer.
func (f *FGA) AddNetworkACL(ctx context.Context, projectName string, networkACLName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkACL(projectName, networkACLName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteNetworkACL deletes a network ACL from the authorizer.
func (f *FGA) DeleteNetworkACL(ctx context.Context, projectName string, networkACLName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkACL(projectName, networkACLName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameNetworkACL renames a network ACL in the authorizer.
func (f *FGA) RenameNetworkACL(ctx context.Context, projectName string, oldNetworkACLName string, newNetworkACLName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkACL(projectName, newNetworkACLName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkACL(projectName, oldNetworkACLName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddNetworkAddressSet adds a network address set to the authorization model.
func (f *FGA) AddNetworkAddressSet(ctx context.Context, projectName string, networkAddressSetName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkAddressSet(projectName, networkAddressSetName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteNetworkAddressSet removes a network address set from the authorization model.
func (f *FGA) DeleteNetworkAddressSet(ctx context.Context, projectName string, networkAddressSetName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkAddressSet(projectName, networkAddressSetName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameNetworkAddressSet renames an existing network address set in the authorization model.
func (f *FGA) RenameNetworkAddressSet(ctx context.Context, projectName string, oldNetworkAddressSetName string, newNetworkAddressSetName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkAddressSet(projectName, newNetworkAddressSetName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectNetworkAddressSet(projectName, oldNetworkAddressSetName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddProfile is a no-op.
func (f *FGA) AddProfile(ctx context.Context, projectName string, profileName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, profileName).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteProfile deletes a profile from the authorizer.
func (f *FGA) DeleteProfile(ctx context.Context, projectName string, profileName string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, profileName).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameProfile renames a profile in the authorizer.
func (f *FGA) RenameProfile(ctx context.Context, projectName string, oldProfileName string, newProfileName string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, newProfileName).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectProfile(projectName, oldProfileName).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddStoragePoolVolume adds a storage volume to the authorizer.
func (f *FGA) AddStoragePoolVolume(ctx context.Context, projectName string, storagePoolName string, storageVolumeType string, storageVolumeName string, storageVolumeLocation string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageVolume(projectName, storagePoolName, storageVolumeType, storageVolumeName, storageVolumeLocation).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteStoragePoolVolume deletes a storage volume from the authorizer.
func (f *FGA) DeleteStoragePoolVolume(ctx context.Context, projectName string, storagePoolName string, storageVolumeType string, storageVolumeName string, storageVolumeLocation string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageVolume(projectName, storagePoolName, storageVolumeType, storageVolumeName, storageVolumeLocation).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// RenameStoragePoolVolume renames a storage volume in the authorizer.
func (f *FGA) RenameStoragePoolVolume(ctx context.Context, projectName string, storagePoolName string, storageVolumeType string, oldStorageVolumeName string, newStorageVolumeName string, storageVolumeLocation string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageVolume(projectName, storagePoolName, storageVolumeType, newStorageVolumeName, storageVolumeLocation).String(),
		},
	}

	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageVolume(projectName, storagePoolName, storageVolumeType, oldStorageVolumeName, storageVolumeLocation).String(),
		},
	}

	return f.updateTuples(ctx, writes, deletions)
}

// AddStorageBucket adds a storage bucket to the authorizer.
func (f *FGA) AddStorageBucket(ctx context.Context, projectName string, storagePoolName string, storageBucketName string, storageBucketLocation string) error {
	writes := []client.ClientTupleKey{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageBucket(projectName, storagePoolName, storageBucketName, storageBucketLocation).String(),
		},
	}

	return f.updateTuples(ctx, writes, nil)
}

// DeleteStorageBucket deletes a storage bucket from the authorizer.
func (f *FGA) DeleteStorageBucket(ctx context.Context, projectName string, storagePoolName string, storageBucketName string, storageBucketLocation string) error {
	deletions := []client.ClientTupleKeyWithoutCondition{
		{
			User:     ObjectProject(projectName).String(),
			Relation: relationProject,
			Object:   ObjectStorageBucket(projectName, storagePoolName, storageBucketName, storageBucketLocation).String(),
		},
	}

	return f.updateTuples(ctx, nil, deletions)
}

// updateTuples sends an object update to OpenFGA if it's currently online.
func (f *FGA) updateTuples(ctx context.Context, writes []client.ClientTupleKey, deletions []client.ClientTupleKeyWithoutCondition) error {
	// If offline, skip updating as a full sync will happen after connection.
	f.onlineMu.Lock()
	defer f.onlineMu.Unlock()
	if !f.online {
		return nil
	}

	if len(writes) == 0 && len(deletions) == 0 {
		return nil
	}

	return f.sendTuples(ctx, writes, deletions)
}

// sendTuples directly sends the write/deletion tuples to OpenFGA.
func (f *FGA) sendTuples(ctx context.Context, writes []client.ClientTupleKey, deletions []client.ClientTupleKeyWithoutCondition) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	opts := client.ClientWriteOptions{
		Transaction: &client.TransactionOptions{
			Disable:             true,
			MaxParallelRequests: 5,
			MaxPerChunk:         50,
		},
	}

	body := client.ClientWriteRequest{}

	if writes != nil {
		body.Writes = writes
	} else {
		body.Writes = []client.ClientTupleKey{}
	}

	if deletions != nil {
		body.Deletes = deletions
	} else {
		body.Deletes = []openfga.TupleKeyWithoutCondition{}
	}

	clientWriteResponse, err := f.client.Write(ctx).Options(opts).Body(body).Execute()
	if err != nil {
		return fmt.Errorf("Failed to write to OpenFGA store: %w", err)
	}

	for _, write := range clientWriteResponse.Writes {
		if write.Error != nil {
			return fmt.Errorf("Failed to write tuple to OpenFGA store (user: %q; relation: %q; object: %q): %w", write.TupleKey.User, write.TupleKey.Relation, write.TupleKey.Object, write.Error)
		}
	}

	for _, deletion := range clientWriteResponse.Deletes {
		if deletion.Error != nil {
			return fmt.Errorf("Failed to delete tuple from OpenFGA store (user: %q; relation: %q; object: %q): %w", deletion.TupleKey.User, deletion.TupleKey.Relation, deletion.TupleKey.Object, deletion.Error)
		}
	}

	return nil
}

func (f *FGA) projectObjects(ctx context.Context, projectName string) ([]string, error) {
	objectTypes := []ObjectType{
		ObjectTypeInstance,
		ObjectTypeImage,
		ObjectTypeImageAlias,
		ObjectTypeNetwork,
		ObjectTypeNetworkACL,
		ObjectTypeNetworkAddressSet,
		ObjectTypeNetworkZone,
		ObjectTypeProfile,
		ObjectTypeStorageVolume,
		ObjectTypeStorageBucket,
	}

	var allObjects []string
	projectObjectString := ObjectProject(projectName).String()
	for _, objectType := range objectTypes {
		resp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
			User:     projectObjectString,
			Relation: relationProject,
			Type:     string(objectType),
		}).Execute()
		if err != nil {
			return nil, err
		}

		allObjects = append(allObjects, resp.GetObjects()...)
	}

	return allObjects, nil
}

func (f *FGA) syncResources(ctx context.Context, resources Resources) error {
	var writes []client.ClientTupleKey
	var deletions []client.ClientTupleKeyWithoutCondition

	// Helper function for diffing local objects with those in OpenFGA. These are appended to the writes and deletions
	// slices as appropriate. If the given relation is relationProject, we need to construct a project object for the
	// "user" field. The project is calculated from the object we are inspecting.
	diffObjects := func(relation string, remoteObjectStrs []string, localObjects []Object) error {
		user := ObjectServer().String()

		for _, localObject := range localObjects {
			if !slices.Contains(remoteObjectStrs, localObject.String()) {
				if relation == relationProject {
					user = ObjectProject(localObject.Project()).String()
				}

				writes = append(writes, client.ClientTupleKey{
					User:     user,
					Relation: relation,
					Object:   localObject.String(),
				})
			}
		}

		for _, remoteObjectStr := range remoteObjectStrs {
			remoteObject, err := ObjectFromString(remoteObjectStr)
			if err != nil {
				return err
			}

			if !slices.Contains(localObjects, remoteObject) {
				if relation == relationProject {
					user = ObjectProject(remoteObject.Project()).String()
				}

				deletions = append(deletions, client.ClientTupleKeyWithoutCondition{
					User:     user,
					Relation: relation,
					Object:   remoteObject.String(),
				})
			}
		}

		return nil
	}

	// List the certificates we have added to OpenFGA already.
	certificatesResp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
		User:     ObjectServer().String(),
		Relation: relationServer,
		Type:     string(ObjectTypeCertificate),
	}).Execute()
	if err != nil {
		return err
	}

	// Compare with local certificates.
	err = diffObjects(relationServer, certificatesResp.GetObjects(), resources.CertificateObjects)
	if err != nil {
		return err
	}

	// List the network integrations we have added to OpenFGA already.
	networkIntegrationsResp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
		User:     ObjectServer().String(),
		Relation: relationServer,
		Type:     string(ObjectTypeNetworkIntegration),
	}).Execute()
	if err != nil {
		return err
	}

	// Compare with local network integrations.
	err = diffObjects(relationServer, networkIntegrationsResp.GetObjects(), resources.NetworkIntegrationObjects)
	if err != nil {
		return err
	}

	// List the storage pools we have added to OpenFGA already.
	storagePoolsResp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
		User:     ObjectServer().String(),
		Relation: relationServer,
		Type:     string(ObjectTypeStoragePool),
	}).Execute()
	if err != nil {
		return err
	}

	// Compare with local storage pools.
	err = diffObjects(relationServer, storagePoolsResp.GetObjects(), resources.StoragePoolObjects)
	if err != nil {
		return err
	}

	// List the projects we have added to OpenFGA already.
	projectsResp, err := f.client.ListObjects(ctx).Body(client.ClientListObjectsRequest{
		User:     ObjectServer().String(),
		Relation: relationServer,
		Type:     string(ObjectTypeProject),
	}).Execute()
	if err != nil {
		return err
	}

	// Compare with local projects.
	remoteProjectObjectStrs := projectsResp.GetObjects()
	err = diffObjects(relationServer, remoteProjectObjectStrs, resources.ProjectObjects)
	if err != nil {
		return err
	}

	// Get a slice of project level resources for all projects.
	var remoteProjectResourceObjectStrs []string
	for _, remoteProjectObjectStr := range remoteProjectObjectStrs {
		remoteProjectObject, err := ObjectFromString(remoteProjectObjectStr)
		if err != nil {
			return err
		}

		// project level resources just for this project.
		remoteProjectResources, err := f.projectObjects(ctx, remoteProjectObject.Project())
		if err != nil {
			return err
		}

		remoteProjectResourceObjectStrs = append(remoteProjectResourceObjectStrs, remoteProjectResources...)
	}

	// Compose a slice of all project level objects from the given Resources.
	localProjectObjects := append(resources.ImageObjects, resources.ImageAliasObjects...)
	localProjectObjects = append(localProjectObjects, resources.InstanceObjects...)
	localProjectObjects = append(localProjectObjects, resources.NetworkObjects...)
	localProjectObjects = append(localProjectObjects, resources.NetworkZoneObjects...)
	localProjectObjects = append(localProjectObjects, resources.NetworkACLObjects...)
	localProjectObjects = append(localProjectObjects, resources.NetworkAddressSetObjects...)
	localProjectObjects = append(localProjectObjects, resources.ProfileObjects...)
	localProjectObjects = append(localProjectObjects, resources.StoragePoolVolumeObjects...)
	localProjectObjects = append(localProjectObjects, resources.StorageBucketObjects...)

	// Perform a diff on the project resource objects.
	err = diffObjects(relationProject, remoteProjectResourceObjectStrs, localProjectObjects)
	if err != nil {
		return err
	}

	// Perform any necessary writes and deletions against the OpenFGA server.
	return f.updateTuples(ctx, writes, deletions)
}

// GetInstanceAccess returns the list of entities who have access to the instance.
func (f *FGA) GetInstanceAccess(ctx context.Context, projectName string, instanceName string) (*api.Access, error) {
	// Get all the entries from OpenFGA.
	entries := map[string]string{}

	userFilters := []openfga.UserTypeFilter{{Type: "user"}}
	relations := []string{"admin", "operator", "user", "viewer"}
	for _, relation := range relations {
		resp, err := f.client.ListUsers(ctx).Body(client.ClientListUsersRequest{
			Object: openfga.FgaObject{
				Type: "instance",
				Id:   fmt.Sprintf("%s/%s", projectName, instanceName),
			},
			Relation:    relation,
			UserFilters: userFilters,
		}).Execute()
		if err != nil {
			var fgaAPIErr openfga.FgaApiValidationError
			ok := errors.As(err, &fgaAPIErr)
			if !ok || fgaAPIErr.ResponseCode() != openfga.ERRORCODE_RELATION_NOT_FOUND {
				var fgaNotFoundErr openfga.FgaApiNotFoundError
				ok := errors.As(err, &fgaNotFoundErr)
				if ok && fgaNotFoundErr.ResponseCode() == openfga.NOTFOUNDERRORCODE_UNDEFINED_ENDPOINT {
					return nil, errors.New("OpenFGA server doesn't support listing users")
				}

				return nil, fmt.Errorf("Failed to list objects with relation %q: %w: %T", relation, err, err)
			}
		}

		for _, user := range resp.GetUsers() {
			obj := user.GetObject()
			if obj.Id == "" {
				continue
			}

			_, ok := entries[obj.Id]
			if !ok {
				entries[obj.Id] = relation
			}
		}
	}

	// Convert to our access records.
	access := api.Access{}
	for user, relation := range entries {
		access = append(access, api.AccessEntry{
			Identifier: user,
			Role:       relation,
			Provider:   "openfga",
		})
	}

	return &access, nil
}

// GetProjectAccess returns the list of entities who have access to the project.
func (f *FGA) GetProjectAccess(ctx context.Context, projectName string) (*api.Access, error) {
	// Get all the entries from OpenFGA.
	entries := map[string]string{}

	userFilters := []openfga.UserTypeFilter{{Type: "user"}}
	relations := []string{"admin", "operator", "user", "viewer"}
	for _, relation := range relations {
		resp, err := f.client.ListUsers(ctx).Body(client.ClientListUsersRequest{
			Object: openfga.FgaObject{
				Type: "project",
				Id:   projectName,
			},
			Relation:    relation,
			UserFilters: userFilters,
		}).Execute()
		if err != nil {
			var fgaAPIErr openfga.FgaApiValidationError
			ok := errors.As(err, &fgaAPIErr)
			if !ok || fgaAPIErr.ResponseCode() != openfga.ERRORCODE_RELATION_NOT_FOUND {
				var fgaNotFoundErr openfga.FgaApiNotFoundError
				ok := errors.As(err, &fgaNotFoundErr)
				if ok && fgaNotFoundErr.ResponseCode() == openfga.NOTFOUNDERRORCODE_UNDEFINED_ENDPOINT {
					return nil, errors.New("OpenFGA server doesn't support listing users")
				}

				return nil, fmt.Errorf("Failed to list objects with relation %q: %w: %T", relation, err, err)
			}
		}

		for _, user := range resp.GetUsers() {
			obj := user.GetObject()
			if obj.Id == "" {
				continue
			}

			_, ok := entries[obj.Id]
			if !ok {
				entries[obj.Id] = relation
			}
		}
	}

	// Convert to our access records.
	access := api.Access{}
	for user, relation := range entries {
		access = append(access, api.AccessEntry{
			Identifier: user,
			Role:       relation,
			Provider:   "openfga",
		})
	}

	return &access, nil
}
