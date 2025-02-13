/*
Copyright 2022 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"fmt"
	"path"

	"github.com/kcp-dev/logicalcluster"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apiextensions-apiserver/pkg/controller/openapi/builder"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	"k8s.io/apiserver/pkg/endpoints/openapi"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilopenapi "k8s.io/apiserver/pkg/util/openapi"
	klog "k8s.io/klog/v2"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"

	apiresourcev1alpha1 "github.com/kcp-dev/kcp/pkg/apis/apiresource/v1alpha1"
	"github.com/kcp-dev/kcp/pkg/virtual/framework/dynamic/apidefinition"
)

var _ apidefinition.APIDefinition = (*servingInfo)(nil)

// RestProviderFunc is the type of a function that builds REST storage implementations for the main resource and sub-resources, based on informations passed by the resource handler about a given API.
type RestProviderFunc func(resource schema.GroupVersionResource, kind schema.GroupVersionKind, listKind schema.GroupVersionKind, typer runtime.ObjectTyper, tableConvertor rest.TableConvertor, namespaceScoped bool, schemaValidator *validate.SchemaValidator, subresourcesSchemaValidator map[string]*validate.SchemaValidator, structuralSchema *structuralschema.Structural) (mainStorage rest.Storage, subresourceStorages map[string]rest.Storage)

// CreateServingInfoFor method can be used by external components at any time to create an APIDefinition and add it to an APISetRetriever
func CreateServingInfoFor(genericConfig genericapiserver.CompletedConfig, logicalClusterName logicalcluster.Name, apiResourceSpec *apiresourcev1alpha1.CommonAPIResourceSpec, restProvider RestProviderFunc) (apidefinition.APIDefinition, error) {
	equivalentResourceRegistry := runtime.NewEquivalentResourceRegistry()

	v1OpenAPISchema, err := apiResourceSpec.GetSchema()
	if err != nil {
		return nil, err
	}
	internalSchema := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1OpenAPISchema, internalSchema, nil); err != nil {
		return nil, fmt.Errorf("failed converting CRD validation to internal version: %w", err)
	}
	structuralSchema, err := structuralschema.NewStructural(internalSchema)
	if err != nil {
		// This should never happen. If it does, it is a programming error.
		utilruntime.HandleError(fmt.Errorf("failed to convert schema to structural: %w", err))
		return nil, fmt.Errorf("the server could not properly serve the CR schema") // validation should avoid this
	}

	// we don't own structuralSchema completely, e.g. defaults are not deep-copied. So better make a copy here.
	structuralSchema = structuralSchema.DeepCopy()

	resource := schema.GroupVersionResource{Group: apiResourceSpec.GroupVersion.Group, Version: apiResourceSpec.GroupVersion.Version, Resource: apiResourceSpec.Plural}
	kind := schema.GroupVersionKind{Group: apiResourceSpec.GroupVersion.Group, Version: apiResourceSpec.GroupVersion.Version, Kind: apiResourceSpec.Kind}
	listKind := schema.GroupVersionKind{Group: apiResourceSpec.GroupVersion.Group, Version: apiResourceSpec.GroupVersion.Version, Kind: apiResourceSpec.ListKind}

	if err := structuraldefaulting.PruneDefaults(structuralSchema); err != nil {
		// This should never happen. If it does, it is a programming error.
		utilruntime.HandleError(fmt.Errorf("failed to prune defaults for schema %s|%s: %w", logicalClusterName.String(), resource.String(), err))
		return nil, fmt.Errorf("the server could not properly serve the CR schema") // validation should avoid this
	}

	s, err := buildOpenAPIV2(
		apiResourceSpec,
		builder.Options{
			V2: true,
			SkipFilterSchemaForKubectlOpenAPIV2Validation: true,
			StripValueValidation:                          true,
			StripNullable:                                 true,
			AllowNonStructural:                            false})
	if err != nil {
		return nil, err
	}

	var modelsByGKV openapi.ModelsByGKV

	openAPIModels, err := utilopenapi.ToProtoModels(s)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error building openapi models for %s: %w", kind.String(), err))
		openAPIModels = nil
	} else {
		modelsByGKV, err = openapi.GetModelsByGKV(openAPIModels)
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("error gathering openapi models by GKV for %s: %w", kind.String(), err))
			modelsByGKV = nil
		}
	}
	var typeConverter fieldmanager.TypeConverter = fieldmanager.DeducedTypeConverter{}
	if openAPIModels != nil {
		typeConverter, err = fieldmanager.NewTypeConverter(openAPIModels, false)
		if err != nil {
			return nil, err
		}
	}

	safeConverter, unsafeConverter := &nopConverter{}, &nopConverter{}
	if err != nil {
		return nil, err
	}

	// In addition to Unstructured objects (Custom Resources), we also may sometimes need to
	// decode unversioned Options objects, so we delegate to parameterScheme for such types.
	parameterScheme := runtime.NewScheme()
	parameterScheme.AddUnversionedTypes(schema.GroupVersion{Group: apiResourceSpec.GroupVersion.Group, Version: apiResourceSpec.GroupVersion.Version},
		&metav1.ListOptions{},
		&metav1.GetOptions{},
		&metav1.DeleteOptions{},
	)
	parameterCodec := runtime.NewParameterCodec(parameterScheme)

	equivalentResourceRegistry.RegisterKindFor(resource, "", kind)

	typer := apiextensionsapiserver.NewUnstructuredObjectTyper(parameterScheme)
	creator := apiextensionsapiserver.UnstructuredCreator{}

	internalValidationSchema := &apiextensionsinternal.CustomResourceValidation{
		OpenAPIV3Schema: internalSchema,
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(internalValidationSchema)
	if err != nil {
		return nil, err
	}

	subResourcesValidators := map[string]*validate.SchemaValidator{}

	if subresources := apiResourceSpec.SubResources; subresources != nil && subresources.Contains("status") {
		var statusValidator *validate.SchemaValidator
		equivalentResourceRegistry.RegisterKindFor(resource, "status", kind)
		// for the status subresource, validate only against the status schema
		if internalValidationSchema != nil && internalValidationSchema.OpenAPIV3Schema != nil && internalValidationSchema.OpenAPIV3Schema.Properties != nil {
			if statusSchema, ok := internalValidationSchema.OpenAPIV3Schema.Properties["status"]; ok {
				openapiSchema := &spec.Schema{}
				if err := apiservervalidation.ConvertJSONSchemaPropsWithPostProcess(&statusSchema, openapiSchema, apiservervalidation.StripUnsupportedFormatsPostProcess); err != nil {
					return nil, err
				}
				statusValidator = validate.NewSchemaValidator(openapiSchema, nil, "", strfmt.Default)
			}
		}
		subResourcesValidators["status"] = statusValidator
	}

	table, err := tableconvertor.New(apiResourceSpec.ColumnDefinitions.ToCustomResourceColumnDefinitions())
	if err != nil {
		klog.V(2).Infof("The CRD for %s|%s has an invalid printer specification, falling back to default printing: %v", logicalClusterName.String(), kind.String(), err)
	}

	storage, subresourceStorages := restProvider(
		resource,
		kind,
		listKind,
		typer,
		table,
		apiResourceSpec.Scope == apiextensionsv1.NamespaceScoped,
		validator,
		subResourcesValidators,
		structuralSchema,
	)

	selfLinkPrefixPrefix := path.Join("apis", apiResourceSpec.GroupVersion.Group, apiResourceSpec.GroupVersion.Version)
	if apiResourceSpec.GroupVersion.Group == "" {
		selfLinkPrefixPrefix = path.Join("api", apiResourceSpec.GroupVersion.Version)
	}
	selfLinkPrefix := ""
	switch apiResourceSpec.Scope {
	case apiextensionsv1.ClusterScoped:
		selfLinkPrefix = "/" + selfLinkPrefixPrefix + "/" + apiResourceSpec.Plural + "/"
	case apiextensionsv1.NamespaceScoped:
		selfLinkPrefix = "/" + selfLinkPrefixPrefix + "/namespaces/"
	}

	clusterScoped := apiResourceSpec.Scope == apiextensionsv1.ClusterScoped

	// CRDs explicitly do not support protobuf, but some objects returned by the API server do
	negotiatedSerializer := apiextensionsapiserver.NewUnstructuredNegotiatedSerializer(
		typer,
		creator,
		safeConverter,
		map[string]*structuralschema.Structural{kind.Version: structuralSchema},
		kind.GroupKind(),
		false,
	)
	var standardSerializers []runtime.SerializerInfo
	for _, s := range negotiatedSerializer.SupportedMediaTypes() {
		if s.MediaType == runtime.ContentTypeProtobuf {
			continue
		}
		standardSerializers = append(standardSerializers, s)
	}

	requestScope := &handlers.RequestScope{
		Namer: handlers.ContextBasedNaming{
			SelfLinker:         meta.NewAccessor(),
			ClusterScoped:      clusterScoped,
			SelfLinkPathPrefix: selfLinkPrefix,
		},
		Serializer:          negotiatedSerializer,
		ParameterCodec:      parameterCodec,
		StandardSerializers: standardSerializers,
		Creater:             creator,
		Convertor:           safeConverter,
		Defaulter: apiextensionsapiserver.NewUnstructuredDefaulter(
			parameterScheme,
			map[string]*structuralschema.Structural{kind.Version: structuralSchema},
			kind.GroupKind(),
		),
		Typer:                    typer,
		UnsafeConvertor:          unsafeConverter,
		EquivalentResourceMapper: equivalentResourceRegistry,
		Resource:                 resource,
		Kind:                     kind,
		HubGroupVersion:          kind.GroupVersion(),
		MetaGroupVersion:         metav1.SchemeGroupVersion,
		TableConvertor:           table,
		Authorizer:               genericConfig.Authorization.Authorizer,
		MaxRequestBodyBytes:      genericConfig.MaxRequestBodyBytes,
		OpenapiModels:            modelsByGKV,
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.ServerSideApply) {
		if withResetFields, canGetResetFields := storage.(rest.ResetFieldsStrategy); canGetResetFields {
			resetFields := withResetFields.GetResetFields()
			reqScope := *requestScope
			reqScope, err = apiextensionsapiserver.ScopeWithFieldManager(
				typeConverter,
				reqScope,
				resetFields,
				"",
			)
			if err != nil {
				return nil, err
			}
			requestScope = &reqScope
		} else {
			return nil, fmt.Errorf("storage for resource %q should define GetResetFields", kind.String())
		}
	}

	var statusScope handlers.RequestScope
	statusStorage, statusEnabled := subresourceStorages["status"]
	if statusEnabled {
		// shallow copy
		statusScope = *requestScope
		statusScope.Subresource = "status"
		statusScope.Namer = handlers.ContextBasedNaming{
			SelfLinker:         meta.NewAccessor(),
			ClusterScoped:      clusterScoped,
			SelfLinkPathPrefix: selfLinkPrefix,
			SelfLinkPathSuffix: "/status",
		}

		if utilfeature.DefaultFeatureGate.Enabled(features.ServerSideApply) {
			if withResetFields, canGetResetFields := statusStorage.(rest.ResetFieldsStrategy); canGetResetFields {
				resetFields := withResetFields.GetResetFields()
				statusScope, err = apiextensionsapiserver.ScopeWithFieldManager(
					typeConverter,
					statusScope,
					resetFields,
					"status",
				)
				if err != nil {
					return nil, err
				}
			} else {
				return nil, fmt.Errorf("storage for resource %q status should define GetResetFields", kind.String())
			}
		}
	}

	ret := &servingInfo{
		logicalClusterName: logicalClusterName,
		apiResourceSpec:    apiResourceSpec,
		storage:            storage,
		statusStorage:      statusStorage,
		requestScope:       requestScope,
		statusRequestScope: &statusScope,
	}

	return ret, nil
}

// servingInfo stores enough information to serve the storage for the apiResourceSpec
type servingInfo struct {
	logicalClusterName logicalcluster.Name
	apiResourceSpec    *apiresourcev1alpha1.CommonAPIResourceSpec

	storage       rest.Storage
	statusStorage rest.Storage

	requestScope       *handlers.RequestScope
	statusRequestScope *handlers.RequestScope
}

// Implement APIDefinition interface

func (apiDef *servingInfo) GetAPIResourceSpec() *apiresourcev1alpha1.CommonAPIResourceSpec {
	return apiDef.apiResourceSpec
}
func (apiDef *servingInfo) GetClusterName() logicalcluster.Name {
	return apiDef.logicalClusterName
}
func (apiDef *servingInfo) GetStorage() rest.Storage {
	return apiDef.storage
}
func (apiDef *servingInfo) GetSubResourceStorage(subresource string) rest.Storage {
	if subresource == "status" {
		return apiDef.statusStorage
	}
	return nil
}
func (apiDef *servingInfo) GetRequestScope() *handlers.RequestScope {
	return apiDef.requestScope
}
func (apiDef *servingInfo) GetSubResourceRequestScope(subresource string) *handlers.RequestScope {
	if subresource == "status" {
		return apiDef.statusRequestScope
	}
	return nil
}
func (apiDef *servingInfo) TearDown() {
}

var _ runtime.ObjectConvertor = nopConverter{}

type nopConverter struct{}

func (u nopConverter) Convert(in, out, context interface{}) error {
	sv, err := conversion.EnforcePtr(in)
	if err != nil {
		return err
	}
	dv, err := conversion.EnforcePtr(out)
	if err != nil {
		return err
	}
	dv.Set(sv)
	return nil
}
func (u nopConverter) ConvertToVersion(in runtime.Object, gv runtime.GroupVersioner) (out runtime.Object, err error) {
	return in, nil
}
func (u nopConverter) ConvertFieldLabel(gvk schema.GroupVersionKind, label, value string) (string, string, error) {
	return label, value, nil
}

// buildOpenAPIV2 builds OpenAPI v2 for the given apiResourceSpec
func buildOpenAPIV2(apiResourceSpec *apiresourcev1alpha1.CommonAPIResourceSpec, opts builder.Options) (*spec.Swagger, error) {
	version := apiResourceSpec.GroupVersion.Version
	schema, err := apiResourceSpec.GetSchema()
	if err != nil {
		return nil, err
	}
	var subResources apiextensionsv1.CustomResourceSubresources
	for _, subResource := range apiResourceSpec.SubResources {
		if subResource.Name == "scale" {
			subResources.Scale = &apiextensionsv1.CustomResourceSubresourceScale{
				SpecReplicasPath:   ".spec.replicas",
				StatusReplicasPath: ".status.replicas",
			}
		}
		if subResource.Name == "status" {
			subResources.Status = &apiextensionsv1.CustomResourceSubresourceStatus{}
		}
	}
	crd := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: apiResourceSpec.GroupVersion.Group,
			Names: apiResourceSpec.CustomResourceDefinitionNames,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name: version,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: schema,
					},
					Subresources: &subResources,
				},
			},
			Scope: apiResourceSpec.Scope,
		},
	}
	return builder.BuildOpenAPIV2(crd, version, opts)
}
