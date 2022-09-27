/*
Copyright SecureKey Technologies Inc. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

//go:generate oapi-codegen --config=openapi.cfg.yaml ../../../../docs/v1/openapi.yaml
//go:generate mockgen -destination controller_mocks_test.go -self_package mocks -package issuer -source=controller.go -mock_names profileService=MockProfileService,kmsRegistry=MockKMSRegistry

package issuer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyperledger/aries-framework-go/pkg/doc/cm"
	"github.com/hyperledger/aries-framework-go/pkg/doc/verifiable"
	arieskms "github.com/hyperledger/aries-framework-go/pkg/kms"
	"github.com/labstack/echo/v4"
	"github.com/piprate/json-gold/ld"

	"github.com/trustbloc/vcs/pkg/doc/vc"
	"github.com/trustbloc/vcs/pkg/doc/vc/crypto"
	cslstatus "github.com/trustbloc/vcs/pkg/doc/vc/status/csl"
	vcsverifiable "github.com/trustbloc/vcs/pkg/doc/verifiable"
	"github.com/trustbloc/vcs/pkg/issuer"
	"github.com/trustbloc/vcs/pkg/kms"
	"github.com/trustbloc/vcs/pkg/restapi/resterr"
	"github.com/trustbloc/vcs/pkg/restapi/v1/common"
	"github.com/trustbloc/vcs/pkg/restapi/v1/util"
)

const (
	issuerProfileSvcComponent  = "issuer.ProfileService"
	issuerProfileCtrlComponent = "issuer.Controller"
	issuerKMSRegistryComponent = "kms.Registry"
	vcConfigKeyType            = "vcConfig.keyType"
	vcConfigFormat             = "vcConfig.format"
	vcConfigSigningAlgorithm   = "vcConfig.signingAlgorithm"
	vcConfigDidMethod          = "vcConfig.didMethod"
	profileCredentialManifests = "credentialManifests" //nolint: gosec
	profileOrganizationID      = "organizationID"
	profileName                = "name"
)

var _ ServerInterface = (*Controller)(nil) // make sure Controller implements ServerInterface

type kmsManager = kms.VCSKeyManager

type kmsRegistry interface {
	GetKeyManager(config *kms.Config) (kmsManager, error)
}

type profileService interface {
	Create(profile *issuer.Profile,
		credentialManifests []*cm.CredentialManifest) (*issuer.Profile, error)
	Update(profile *issuer.ProfileUpdate) error
	Delete(profileID issuer.ProfileID) error
	GetProfile(profileID issuer.ProfileID) (*issuer.Profile, error)
	ActivateProfile(profileID issuer.ProfileID) error
	DeactivateProfile(profileID issuer.ProfileID) error
	GetAllProfiles(orgID string) ([]*issuer.Profile, error)
}

type issueCredentialService interface {
	IssueCredential(credential *verifiable.Credential,
		issuerSigningOpts []crypto.SigningOpts,
		profile *issuer.Profile) (*verifiable.Credential, error)
}

type Config struct {
	ProfileSvc             profileService
	KMSRegistry            kmsRegistry
	DocumentLoader         ld.DocumentLoader
	IssueCredentialService issueCredentialService
}

// Controller for Issuer Profile Management API.
type Controller struct {
	profileSvc             profileService
	kmsRegistry            kmsRegistry
	documentLoader         ld.DocumentLoader
	issueCredentialService issueCredentialService
}

// NewController creates a new controller for Issuer Profile Management API.
func NewController(config *Config) *Controller {
	return &Controller{
		profileSvc:             config.ProfileSvc,
		kmsRegistry:            config.KMSRegistry,
		documentLoader:         config.DocumentLoader,
		issueCredentialService: config.IssueCredentialService,
	}
}

// PostIssuerProfiles creates a new issuer profile.
// POST /issuer/profiles.
func (c *Controller) PostIssuerProfiles(ctx echo.Context) error {
	var body CreateIssuerProfileData

	if err := util.ReadBody(ctx, &body); err != nil {
		return err
	}

	return util.WriteOutput(ctx)(c.createProfile(ctx, &body))
}

func (c *Controller) createProfile(ctx echo.Context, body *CreateIssuerProfileData) (*IssuerProfile, error) {
	orgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return nil, err
	}

	profile, err := c.validateCreateProfileData(body, orgID)
	if err != nil {
		return nil, err
	}

	credentialManifests, err := c.validateCredentialManifests(body.CredentialManifests)
	if err != nil {
		return nil, err
	}

	createdProfile, err := c.profileSvc.Create(profile, credentialManifests)
	if errors.Is(err, issuer.ErrProfileNameDuplication) {
		return nil, resterr.NewValidationError(resterr.AlreadyExist, profileName, err)
	}

	if err != nil {
		return nil, resterr.NewSystemError(issuerProfileSvcComponent, "CreateProfile", err)
	}

	return c.mapToIssuerProfile(createdProfile)
}

// DeleteIssuerProfilesProfileID deletes profile.
// DELETE /issuer/profiles/{profileID}.
func (c *Controller) DeleteIssuerProfilesProfileID(ctx echo.Context, profileID string) error {
	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return err
	}

	err = c.profileSvc.Delete(profile.ID)
	if err != nil {
		return resterr.NewSystemError(issuerProfileSvcComponent, "DeleteProfile", err)
	}

	return nil
}

// GetIssuerProfilesProfileID gets a profile by ID.
// GET /issuer/profiles/{profileID}.
func (c *Controller) GetIssuerProfilesProfileID(ctx echo.Context, profileID string) error {
	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return err
	}

	return util.WriteOutput(ctx)(c.mapToIssuerProfile(profile))
}

// PutIssuerProfilesProfileID updates a profile.
// PUT /issuer/profiles/{profileID}.
func (c *Controller) PutIssuerProfilesProfileID(ctx echo.Context, profileID string) error {
	var body UpdateIssuerProfileData

	if err := util.ReadBody(ctx, &body); err != nil {
		return err
	}

	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return err
	}

	err = c.profileSvc.Update(&issuer.ProfileUpdate{
		ID:         profile.ID,
		Name:       strPtrToStr(body.Name),
		URL:        strPtrToStr(body.Url),
		OIDCConfig: body.OidcConfig,
	})
	if err != nil {
		return resterr.NewSystemError(issuerProfileSvcComponent, "UpdateProfile", err)
	}

	updated, err := c.profileSvc.GetProfile(profile.ID)
	if err != nil {
		return resterr.NewSystemError(issuerProfileSvcComponent, "GetProfile", err)
	}

	return util.WriteOutput(ctx)(c.mapToIssuerProfile(updated))
}

// PostIssuerProfilesProfileIDActivate activates a profile.
// POST /issuer/profiles/{profileID}/activate.
func (c *Controller) PostIssuerProfilesProfileIDActivate(ctx echo.Context, profileID string) error {
	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return err
	}

	err = c.profileSvc.ActivateProfile(profile.ID)
	if err != nil {
		return resterr.NewSystemError(issuerProfileSvcComponent, "ActivateProfile", err)
	}

	return nil
}

// PostIssuerProfilesProfileIDDeactivate deactivates a profile.
// POST /issuer/profiles/{profileID}/deactivate.
func (c *Controller) PostIssuerProfilesProfileIDDeactivate(ctx echo.Context, profileID string) error {
	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return err
	}

	err = c.profileSvc.DeactivateProfile(profile.ID)
	if err != nil {
		return resterr.NewSystemError(issuerProfileSvcComponent, "DeactivateProfile", err)
	}

	return nil
}

// PostIssueCredentials issues credentials.
// POST /issuer/profiles/{profileID}/credentials/issue.
func (c *Controller) PostIssueCredentials(ctx echo.Context, profileID string) error {
	var body IssueCredentialData

	if err := util.ReadBody(ctx, &body); err != nil {
		return err
	}

	return util.WriteOutput(ctx)(c.issueCredential(ctx, &body, profileID))
}

func (c *Controller) issueCredential(ctx echo.Context, body *IssueCredentialData,
	profileID string) (*verifiable.Credential, error) {
	oidcOrgID, err := util.GetOrgIDFromOIDC(ctx)
	if err != nil {
		return nil, err
	}

	profile, err := c.accessProfile(profileID, oidcOrgID)
	if err != nil {
		return nil, err
	}

	vcSchema := verifiable.JSONSchemaLoader(verifiable.WithDisableRequiredField("issuanceDate"))

	credential, err := vc.ValidateCredential(body.Credential, []vcsverifiable.Format{profile.VCConfig.Format},
		verifiable.WithDisabledProofCheck(),
		verifiable.WithSchema(vcSchema),
		verifiable.WithJSONLDDocumentLoader(c.documentLoader))
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, "credential", err)
	}

	credOpts, err := validateIssueCredOptions(body.Options)
	if err != nil {
		return nil, err
	}

	signedVC, err := c.issueCredentialService.IssueCredential(credential, credOpts, profile)
	if err != nil {
		return nil, resterr.NewSystemError("IssueCredentialService", "IssueCredential", err)
	}

	return signedVC, nil
}

func (c *Controller) validateCreateProfileData(body *CreateIssuerProfileData, orgID string) (*issuer.Profile, error) {
	if body.OrganizationID != orgID {
		return nil, resterr.NewValidationError(resterr.InvalidValue, profileOrganizationID,
			fmt.Errorf("org id(%s) from oidc not much profile org id(%s)", orgID, body.OrganizationID))
	}

	kmsConfig, err := common.ValidateKMSConfig(body.KmsConfig)
	if err != nil {
		return nil, err
	}

	keyManager, err := c.kmsRegistry.GetKeyManager(kmsConfig)
	if err != nil {
		return nil, resterr.NewSystemError(issuerKMSRegistryComponent, "GetKeyManager", err)
	}

	vcConfig, err := c.validateVCConfig(&body.VcConfig, keyManager.SupportedKeyTypes())
	if err != nil {
		return nil, err
	}

	// TODO: add validation for profile URL
	url := body.Url
	if !strings.HasSuffix(url, "/") {
		url += "/"
	}

	return &issuer.Profile{
		URL:            url,
		Name:           body.Name,
		Active:         true,
		OIDCConfig:     body.OidcConfig,
		OrganizationID: body.OrganizationID,
		VCConfig:       vcConfig,
		KMSConfig:      kmsConfig,
	}, nil
}

func validateIssueCredOptions(options *IssueCredentialOptions) ([]crypto.SigningOpts, error) {
	var signingOpts []crypto.SigningOpts

	if options == nil {
		return signingOpts, nil
	}
	if options.CredentialStatus.Type != "" && options.CredentialStatus.Type != cslstatus.StatusList2021Entry {
		return nil, resterr.NewValidationError(resterr.InvalidValue, "options.credentialStatus",
			fmt.Errorf("not supported credential status type : %s", options.CredentialStatus.Type))
	}

	verificationMethod := options.VerificationMethod

	if verificationMethod != nil {
		signingOpts = append(signingOpts, crypto.WithVerificationMethod(*verificationMethod))
	}

	if options.Created != nil {
		created, err := time.Parse(time.RFC3339, *options.Created)
		if err != nil {
			return nil, resterr.NewValidationError(resterr.InvalidValue, "options.created", err)
		}
		signingOpts = append(signingOpts, crypto.WithCreated(&created))
	}

	if options.Challenge != nil {
		signingOpts = append(signingOpts, crypto.WithChallenge(*options.Challenge))
	}

	if options.Domain != nil {
		signingOpts = append(signingOpts, crypto.WithDomain(*options.Domain))
	}

	return signingOpts, nil
}

func (c *Controller) validateCredentialManifests(credentialManifests *[]map[string]interface{}) (
	[]*cm.CredentialManifest, error) {
	if credentialManifests == nil {
		return nil, nil
	}

	var result []*cm.CredentialManifest

	for _, manifest := range *credentialManifests {
		bytes, err := json.Marshal(manifest)
		if err != nil {
			return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "jsonMarshal",
				fmt.Errorf("validate credentials: marshal json %w", err))
		}

		decoded := &cm.CredentialManifest{}
		err = json.Unmarshal(bytes, decoded)

		if err != nil {
			return nil, resterr.NewValidationError(resterr.InvalidValue, profileCredentialManifests,
				fmt.Errorf("validate credentials: marshal json %w", err))
		}

		result = append(result, decoded)
	}

	return result, nil
}

func (c *Controller) validateVCConfig(vcConfig *VCConfig,
	supportedKeyTypes []arieskms.KeyType) (*issuer.VCConfig, error) {
	vcFormat, err := common.ValidateVCFormat(vcConfig.Format)
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, vcConfigFormat, err)
	}

	signingAlgorithm, err := vcsverifiable.ValidateSignatureAlgorithm(
		vcFormat, vcConfig.SigningAlgorithm, supportedKeyTypes)
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, vcConfigSigningAlgorithm,
			fmt.Errorf("issuer profile service: create profile failed %w", err))
	}

	keyType, err := vcsverifiable.ValidateSignatureKeyType(signingAlgorithm, strPtrToStr(vcConfig.KeyType))
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, vcConfigKeyType,
			fmt.Errorf("issuer profile service: create profile failed %w", err))
	}

	didMethod, err := common.ValidateDIDMethod(vcConfig.DidMethod)
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, vcConfigDidMethod, err)
	}

	signatureRepresentation, err := c.validateSignatureRepresentation(vcConfig.SignatureRepresentation)
	if err != nil {
		return nil, resterr.NewValidationError(resterr.InvalidValue, "signatureRepresentation", err)
	}

	var contexts []string
	if vcConfig.Contexts != nil {
		contexts = *vcConfig.Contexts
	}

	return &issuer.VCConfig{
		Format:                  vcFormat,
		SigningAlgorithm:        signingAlgorithm,
		KeyType:                 keyType,
		DIDMethod:               didMethod,
		SignatureRepresentation: signatureRepresentation,
		Status:                  vcConfig.Status,
		Context:                 contexts,
	}, nil
}

func (c *Controller) accessProfile(profileID string, oidcOrgID string) (*issuer.Profile, error) {
	profile, err := c.profileSvc.GetProfile(profileID)
	if errors.Is(err, issuer.ErrDataNotFound) {
		return nil, resterr.NewValidationError(resterr.DoesntExist, "profile",
			fmt.Errorf("profile with given id %s, dosn't exists", profileID))
	}

	if err != nil {
		return nil, resterr.NewSystemError(issuerProfileSvcComponent, "GetProfile", err)
	}

	// Profiles of other organization is not visible.
	if profile.OrganizationID != oidcOrgID {
		return nil, resterr.NewValidationError(resterr.DoesntExist, "profile",
			fmt.Errorf("profile with given id %s, dosn't exists", profileID))
	}

	return profile, nil
}

func (c *Controller) validateSignatureRepresentation(signatureRepresentation *VCConfigSignatureRepresentation) (
	verifiable.SignatureRepresentation, error) {
	if signatureRepresentation == nil {
		return verifiable.SignatureProofValue, nil
	}

	switch *signatureRepresentation {
	case JWS:
		return verifiable.SignatureJWS, nil
	case ProofValue:
		return verifiable.SignatureProofValue, nil
	}

	return verifiable.SignatureProofValue, fmt.Errorf("unsupported signatureRepresentation %d, use one of next [%s, %s]",
		signatureRepresentation, JWS, ProofValue)
}

func (c *Controller) mapToSignatureRepresentation(signatureRepresentation verifiable.SignatureRepresentation) (
	VCConfigSignatureRepresentation, error) {
	switch signatureRepresentation {
	case verifiable.SignatureJWS:
		return JWS, nil
	case verifiable.SignatureProofValue:
		return ProofValue, nil
	}

	return "", resterr.NewSystemError(issuerProfileCtrlComponent, "mapToDIDMethod",
		fmt.Errorf("signatureRepresentation missmatch %d, rest api supports only [%s, %s]",
			signatureRepresentation, JWS, ProofValue))
}

func (c *Controller) mapToIssuerProfile(p *issuer.Profile) (*IssuerProfile, error) {
	format, err := common.MapToVCFormat(p.VCConfig.Format)
	if err != nil {
		return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "mapToVCFormat", err)
	}

	didMethod, err := common.MapToDIDMethod(p.VCConfig.DIDMethod)
	if err != nil {
		return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "mapToDIDMethod", err)
	}

	signatureRepresentation, err := c.mapToSignatureRepresentation(p.VCConfig.SignatureRepresentation)
	if err != nil {
		return nil, err
	}

	keyType := string(p.VCConfig.KeyType)
	signingAlgorithm := string(p.VCConfig.SigningAlgorithm)

	var kmsConfig *common.KMSConfig

	if p.KMSConfig != nil {
		kmsType, err := common.MapToKMSConfigType(p.KMSConfig.KMSType)
		if err != nil {
			return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "mapToKMSConfigType", err)
		}

		kmsConfig = &common.KMSConfig{
			DbPrefix:          &p.KMSConfig.DBPrefix,
			DbType:            &p.KMSConfig.DBType,
			DbURL:             &p.KMSConfig.DBURL,
			Endpoint:          &p.KMSConfig.Endpoint,
			SecretLockKeyPath: &p.KMSConfig.SecretLockKeyPath,
			Type:              kmsType,
		}
	}

	vcConfig := VCConfig{
		Contexts:                &p.VCConfig.Context,
		DidMethod:               didMethod,
		SignatureRepresentation: &signatureRepresentation,
		Format:                  format,
		KeyType:                 &keyType,
		SigningAlgorithm:        signingAlgorithm,
	}

	if p.SigningDID != nil {
		vcConfig.SigningDID = p.SigningDID.DID
	}

	profile := &IssuerProfile{
		Active:         p.Active,
		Id:             p.ID,
		KmsConfig:      kmsConfig,
		Name:           p.Name,
		OrganizationID: p.OrganizationID,
		Url:            p.URL + p.ID,
		VcConfig:       vcConfig,
	}

	var (
		m  map[string]interface{}
		ok bool
	)

	if p.VCConfig.Status != nil {
		m, ok = p.VCConfig.Status.(map[string]interface{})
		if !ok {
			return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "TypeCast",
				fmt.Errorf("issuer profile vc config status has invalid type"))
		}

		profile.VcConfig.Status = &m
	}

	if p.OIDCConfig != nil {
		m, ok = p.OIDCConfig.(map[string]interface{})
		if !ok {
			return nil, resterr.NewSystemError(issuerProfileCtrlComponent, "TypeCast",
				fmt.Errorf("issuer profile oidc config has invalid type"))
		}

		profile.OidcConfig = &m
	}

	return profile, nil
}

func strPtrToStr(str *string) string {
	if str == nil {
		return ""
	}
	return *str
}
