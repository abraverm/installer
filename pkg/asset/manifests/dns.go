package manifests

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/pkg/errors"

	configv1 "github.com/openshift/api/config/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openshift/installer/pkg/asset"
	"github.com/openshift/installer/pkg/asset/installconfig"
	icaws "github.com/openshift/installer/pkg/asset/installconfig/aws"
	icgcp "github.com/openshift/installer/pkg/asset/installconfig/gcp"
	icibmcloud "github.com/openshift/installer/pkg/asset/installconfig/ibmcloud"
	icpowervs "github.com/openshift/installer/pkg/asset/installconfig/powervs"
	"github.com/openshift/installer/pkg/types"
	alibabacloudtypes "github.com/openshift/installer/pkg/types/alibabacloud"
	awstypes "github.com/openshift/installer/pkg/types/aws"
	azuretypes "github.com/openshift/installer/pkg/types/azure"
	baremetaltypes "github.com/openshift/installer/pkg/types/baremetal"
	gcptypes "github.com/openshift/installer/pkg/types/gcp"
	ibmcloudtypes "github.com/openshift/installer/pkg/types/ibmcloud"
	libvirttypes "github.com/openshift/installer/pkg/types/libvirt"
	nonetypes "github.com/openshift/installer/pkg/types/none"
	nutanixtypes "github.com/openshift/installer/pkg/types/nutanix"
	openstacktypes "github.com/openshift/installer/pkg/types/openstack"
	ovirttypes "github.com/openshift/installer/pkg/types/ovirt"
	powervstypes "github.com/openshift/installer/pkg/types/powervs"
	vspheretypes "github.com/openshift/installer/pkg/types/vsphere"
)

var (
	dnsCfgFilename = filepath.Join(manifestDir, "cluster-dns-02-config.yml")

	combineGCPZoneInfo = func(project, zoneName string) string {
		return fmt.Sprintf("project/%s/managedZones/%s", project, zoneName)
	}
)

// DNS generates the cluster-dns-*.yml files.
type DNS struct {
	FileList []*asset.File
}

var _ asset.WritableAsset = (*DNS)(nil)

// Name returns a human friendly name for the asset.
func (*DNS) Name() string {
	return "DNS Config"
}

// Dependencies returns all of the dependencies directly needed to generate
// the asset.
func (*DNS) Dependencies() []asset.Asset {
	return []asset.Asset{
		&installconfig.InstallConfig{},
		&installconfig.ClusterID{},
		// PlatformCredsCheck just checks the creds (and asks, if needed)
		// We do not actually use it in this asset directly, hence
		// it is put in the dependencies but not fetched in Generate
		&installconfig.PlatformCredsCheck{},
	}
}

// Generate generates the DNS config and its CRD.
func (d *DNS) Generate(dependencies asset.Parents) error {
	installConfig := &installconfig.InstallConfig{}
	clusterID := &installconfig.ClusterID{}
	dependencies.Get(installConfig, clusterID)

	config := &configv1.DNS{
		TypeMeta: metav1.TypeMeta{
			APIVersion: configv1.SchemeGroupVersion.String(),
			Kind:       "DNS",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
			// not namespaced
		},
		Spec: configv1.DNSSpec{
			BaseDomain: installConfig.Config.ClusterDomain(),
		},
	}

	switch installConfig.Config.Platform.Name() {
	case alibabacloudtypes.Name:
		if installConfig.Config.Publish == types.ExternalPublishingStrategy {
			config.Spec.PublicZone = &configv1.DNSZone{
				ID:   installConfig.Config.BaseDomain,
				Tags: map[string]string{"type": "public"},
			}
		}
		// On Alibaba Cloud can be fetched using `ID` as a pre-determined private zone name
		config.Spec.PrivateZone = &configv1.DNSZone{
			ID:   installConfig.Config.ClusterDomain(),
			Tags: map[string]string{"type": "private"},
		}
	case awstypes.Name:
		if installConfig.Config.Publish == types.ExternalPublishingStrategy {
			sess, err := installConfig.AWS.Session(context.TODO())
			if err != nil {
				return errors.Wrap(err, "failed to initialize session")
			}
			zone, err := icaws.GetPublicZone(sess, installConfig.Config.BaseDomain)
			if err != nil {
				return errors.Wrapf(err, "getting public zone for %q", installConfig.Config.BaseDomain)
			}
			config.Spec.PublicZone = &configv1.DNSZone{ID: strings.TrimPrefix(*zone.Id, "/hostedzone/")}
		}
		if hostedZone := installConfig.Config.AWS.HostedZone; hostedZone == "" {
			config.Spec.PrivateZone = &configv1.DNSZone{Tags: map[string]string{
				fmt.Sprintf("kubernetes.io/cluster/%s", clusterID.InfraID): "owned",
				"Name": fmt.Sprintf("%s-int", clusterID.InfraID),
			}}
		} else {
			config.Spec.PrivateZone = &configv1.DNSZone{ID: hostedZone}
		}
	case azuretypes.Name:
		dnsConfig, err := installConfig.Azure.DNSConfig()
		if err != nil {
			return err
		}

		if installConfig.Config.Publish == types.ExternalPublishingStrategy {
			//currently, this guesses the azure resource IDs from known parameter.
			config.Spec.PublicZone = &configv1.DNSZone{
				ID: dnsConfig.GetDNSZoneID(installConfig.Config.Azure.BaseDomainResourceGroupName, installConfig.Config.BaseDomain),
			}
		}
		if installConfig.Azure.CloudName != azuretypes.StackCloud {
			config.Spec.PrivateZone = &configv1.DNSZone{
				ID: dnsConfig.GetPrivateDNSZoneID(installConfig.Config.Azure.ClusterResourceGroupName(clusterID.InfraID), installConfig.Config.ClusterDomain()),
			}
		}
	case gcptypes.Name:

		// Set the public zone
		switch {
		case installConfig.Config.Publish != types.ExternalPublishingStrategy:
			// Do not use a public zone when not publishing externally.
		case installConfig.Config.GCP.PublicDNSZone != nil && installConfig.Config.GCP.PublicDNSZone.ID != "":
			// Use the provided zone if specified.
			zoneID := installConfig.Config.GCP.PublicDNSZone.ID
			if installConfig.Config.GCP.PublicDNSZone.ProjectID != "" && installConfig.Config.GCP.ProjectID != installConfig.Config.GCP.PublicDNSZone.ProjectID {
				zoneID = combineGCPZoneInfo(installConfig.Config.GCP.PublicDNSZone.ProjectID, installConfig.Config.GCP.PublicDNSZone.ID)
			}
			config.Spec.PublicZone = &configv1.DNSZone{ID: zoneID}
		default:
			// Search the project for a zone with the specified base domain.
			zone, err := icgcp.GetPublicZone(context.TODO(), installConfig.Config.GCP.ProjectID, installConfig.Config.BaseDomain)
			if err != nil {
				return errors.Wrapf(err, "failed to get public zone for %q", installConfig.Config.BaseDomain)
			}
			config.Spec.PublicZone = &configv1.DNSZone{ID: zone.Name}
		}

		// Set the private zone
		switch {
		case installConfig.Config.GCP.PrivateDNSZone != nil && installConfig.Config.GCP.PrivateDNSZone.ID != "":
			// Use the provided zone if specified.
			zoneID := installConfig.Config.GCP.PrivateDNSZone.ID
			if installConfig.Config.GCP.PrivateDNSZone.ProjectID != "" && installConfig.Config.GCP.ProjectID != installConfig.Config.GCP.PrivateDNSZone.ProjectID {
				zoneID = combineGCPZoneInfo(installConfig.Config.GCP.PrivateDNSZone.ProjectID, installConfig.Config.GCP.PrivateDNSZone.ID)
			}
			config.Spec.PublicZone = &configv1.DNSZone{ID: zoneID}
		default:
			// Use the installer created private zone.
			config.Spec.PrivateZone = &configv1.DNSZone{ID: fmt.Sprintf("%s-private-zone", clusterID.InfraID)}
		}

	case ibmcloudtypes.Name:
		client, err := icibmcloud.NewClient()
		if err != nil {
			return errors.Wrap(err, "failed to get IBM Cloud client")
		}

		zoneID, err := client.GetDNSZoneIDByName(context.TODO(), installConfig.Config.BaseDomain, installConfig.Config.Publish)
		if err != nil {
			return errors.Wrap(err, "failed to get DNS zone ID")
		}

		if installConfig.Config.Publish == types.ExternalPublishingStrategy {
			config.Spec.PublicZone = &configv1.DNSZone{
				ID: zoneID,
			}
		}
		config.Spec.PrivateZone = &configv1.DNSZone{
			ID: zoneID,
		}
	case powervstypes.Name:
		client, err := icpowervs.NewClient()
		if err != nil {
			return errors.Wrap(err, "failed to get IBM PowerVS client")
		}

		zoneID, err := client.GetDNSZoneIDByName(context.TODO(), installConfig.Config.BaseDomain)
		if err != nil {
			return errors.Wrap(err, "failed to get DNS zone ID")
		}

		if installConfig.Config.Publish == types.ExternalPublishingStrategy {
			config.Spec.PublicZone = &configv1.DNSZone{
				ID: zoneID,
			}
		}
		config.Spec.PrivateZone = &configv1.DNSZone{
			ID: zoneID,
		}
	case libvirttypes.Name, openstacktypes.Name, baremetaltypes.Name, nonetypes.Name, vspheretypes.Name, ovirttypes.Name, nutanixtypes.Name:
	default:
		return errors.New("invalid Platform")
	}

	configData, err := yaml.Marshal(config)
	if err != nil {
		return errors.Wrapf(err, "failed to create %s manifests from InstallConfig", d.Name())
	}

	d.FileList = []*asset.File{
		{
			Filename: dnsCfgFilename,
			Data:     configData,
		},
	}

	return nil
}

// Files returns the files generated by the asset.
func (d *DNS) Files() []*asset.File {
	return d.FileList
}

// Load loads the already-rendered files back from disk.
func (d *DNS) Load(f asset.FileFetcher) (bool, error) {
	return false, nil
}
