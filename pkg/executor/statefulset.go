// Copyright 2019 Hewlett Packard Enterprise Development LP

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package executor

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/bluek8s/kubedirector/pkg/apis/kubedirector/v1beta1"
	kdv1 "github.com/bluek8s/kubedirector/pkg/apis/kubedirector/v1beta1"
	"github.com/bluek8s/kubedirector/pkg/catalog"
	"github.com/bluek8s/kubedirector/pkg/shared"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// defaultMountFolders identifies the set of member filesystems directories
// that will always be placed on shared persistent storage (when available).
var defaultMountFolders = []string{"/etc"}

// appConfigDefaultMountFolders identifies set of member filesystems
// directories that will always be placed on shared persistent storage, if app
// config is provided for a role, and if that role's package has
// useNewSetupLayout set true. Note that the code in getStatefulset assumes
// this is a superset of the defaultMountFolders list.
var appConfigDefaultMountFolders = []string{
	"/etc",
	"/opt/guestconfig",
	"/var/log/guestconfig",
	"/usr/local/bin",
	"/usr/local/lib",
}

// appConfigLegacyDefaultMountFolders identifies set of member filesystems
// directories that will always be placed on shared persistent storage, if app
// config is provided for a role, and if that role's package has
// useNewSetupLayout set false.
var appConfigLegacyDefaultMountFolders = []string{
	"/etc",
	"/opt",
	"/usr",
}

// CreateStatefulSet creates in k8s a zero-replicas statefulset for
// implementing the given role.
func CreateStatefulSet(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
	nativeSystemdSupport bool,
	role *kdv1.Role,
	roleStatus *kdv1.RoleStatus,
) (*appsv1.StatefulSet, error) {

	statefulSet, err := getStatefulset(
		reqLogger,
		cr,
		nativeSystemdSupport,
		role,
		roleStatus,
		0,
	)
	if err != nil {
		return nil, err
	}
	return statefulSet, shared.Create(context.TODO(), statefulSet)
}

// UpdateStatefulSetReplicas modifies an existing statefulset in k8s to have
// the given number of replicas.
func UpdateStatefulSetReplicas(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
	replicas int32,
	statefulSet *appsv1.StatefulSet,
) error {

	*statefulSet.Spec.Replicas = replicas
	err := shared.Update(context.TODO(), statefulSet)
	if err == nil {
		return nil
	}

	// See https://github.com/bluek8s/kubedirector/issues/194
	// Migrate Client().Update() calls back to Patch() calls.

	if !errors.IsConflict(err) {
		shared.LogError(
			reqLogger,
			err,
			cr,
			shared.EventReasonNoEvent,
			"failed to update statefulset",
		)
		return err
	}

	// If there was a resourceVersion conflict then fetch a more
	// recent version of the statefulset and attempt to update that.
	name := types.NamespacedName{
		Namespace: statefulSet.Namespace,
		Name:      statefulSet.Name,
	}
	*statefulSet = appsv1.StatefulSet{}
	err = shared.Get(context.TODO(), name, statefulSet)
	if err != nil {
		shared.LogError(
			reqLogger,
			err,
			cr,
			shared.EventReasonNoEvent,
			"failed to retrieve statefulset",
		)
		return err
	}

	*statefulSet.Spec.Replicas = replicas
	err = shared.Update(context.TODO(), statefulSet)
	if err != nil {
		shared.LogError(
			reqLogger,
			err,
			cr,
			shared.EventReasonNoEvent,
			"failed to update statefulset",
		)
	}
	return err
}

// UpdateStatefulSetNonReplicas examines a current statefulset in k8s and may take
// steps to reconcile it to the desired spec, for properties other than the
// replicas count.
func UpdateStatefulSetNonReplicas(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
	role *kdv1.Role,
	statefulSet *appsv1.StatefulSet,
) error {

	// If no spec, nothing to do.
	if role == nil {
		return nil
	}

	// We could compare the statefulset against the expected statefulset
	// (generated from the CR) and if there is a deviance in properties that we
	// need/expect to be under our control, other than the replicas count,
	// correct them here.

	// For now only checking the owner reference.
	if shared.OwnerReferencesPresent(cr, statefulSet.OwnerReferences) {
		return nil
	}
	shared.LogInfof(
		reqLogger,
		cr,
		shared.EventReasonNoEvent,
		"repairing owner ref on statefulset{%s}",
		statefulSet.Name,
	)
	// So, what to do. Do we add our owner ref to the existing ones? What if
	// something else is claiming to be controller? Probably some stale ref
	// left by a bad backup/restore process? We're just going to nuke any
	// existing owner refs.
	patchedRes := *statefulSet
	patchedRes.OwnerReferences = shared.OwnerReferences(cr)
	patchErr := shared.Patch(
		context.TODO(),
		statefulSet,
		&patchedRes,
	)
	return patchErr
}

// DeleteStatefulSet deletes a statefulset from k8s.
func DeleteStatefulSet(
	namespace string,
	statefulSetName string,
) error {

	toDelete := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName,
			Namespace: namespace,
		},
	}
	return shared.Delete(context.TODO(), toDelete)
}

// getStatefulset composes the spec for creating a statefulset in k8s, based
// on the given virtual cluster CR and for the purposes of implementing the
// given role.
func getStatefulset(
	reqLogger logr.Logger,
	cr *kdv1.KubeDirectorCluster,
	nativeSystemdSupport bool,
	role *kdv1.Role,
	roleStatus *kdv1.RoleStatus,
	replicas int32,
) (*appsv1.StatefulSet, error) {

	labels := labelsForStatefulSet(cr, role)
	podLabels := labelsForPod(cr, role)
	annotations := annotationsForStatefulSet(cr, role)
	podAnnotations := annotationsForPod(cr, role)
	startupScript := getStartupScript(cr)

	portInfoList, portsErr := catalog.PortsForRole(cr, role.Name)
	if portsErr != nil {
		return nil, portsErr
	}

	var endpointPorts []v1.ContainerPort
	for _, portInfo := range portInfoList {
		containerPort := v1.ContainerPort{
			ContainerPort: portInfo.Port,
			Name:          portInfo.ID,
		}
		endpointPorts = append(endpointPorts, containerPort)
	}

	// Check to see if app has requested additional directories to be persisted
	appPersistDirs, persistErr := catalog.AppPersistDirs(cr, role.Name)
	if persistErr != nil {
		return nil, persistErr
	}

	defaultPersistDirs := defaultMountFolders

	// Check if there is an app config package for this role, If so we have
	// to add additional defaults
	setupInfo, setupInfoErr := catalog.AppSetupPackageInfo(cr, role.Name)
	if setupInfoErr != nil {
		return nil, setupInfoErr
	}

	if setupInfo != nil {
		if setupInfo.UseNewSetupLayout {
			defaultPersistDirs = appConfigDefaultMountFolders
		} else {
			defaultPersistDirs = appConfigLegacyDefaultMountFolders
		}
	}

	// Create a combined unique list of directories that have be persisted
	// Start with default mounts
	var maxLen = len(defaultPersistDirs)
	if appPersistDirs != nil {
		maxLen += len(*appPersistDirs)
	}
	persistDirs := make([]string, 0, maxLen)

	// Utility func here to add elements from sourceDirList to persistDirs. An
	// element from sourceDirList will be skipped if it is a subdir of some
	// other element from sourceDirList or otherDirList. If it is a dup of
	// some element from sourceDirList, it will be skipped only if the
	// discovered dup is earlier in the list. If it is a dup of some element
	// from otherDirList, it will be skipped only if checkOtherDups is true.
	addToDirs := func(
		sourceDirList []string,
		otherDirList *[]string,
		checkOtherDups bool,
		sourceDesc string,
	) {

		for sourceIndex, sourceDir := range sourceDirList {
			var coveringDir *string
			absSource, _ := filepath.Abs(sourceDir)
			for otherSourceIndex, otherSourceDir := range sourceDirList {
				absOtherSource, _ := filepath.Abs(otherSourceDir)
				relOther, _ := filepath.Rel(absOtherSource, absSource)
				if !strings.HasPrefix(relOther, "..") {
					if relOther != "." {
						// subdir
						coveringDir = &otherSourceDir
						break
					}
					// dup... we only care if the matched index is earlier
					if otherSourceIndex < sourceIndex {
						coveringDir = &otherSourceDir
						break
					}
				}
			}
			if (coveringDir == nil) && (otherDirList != nil) {
				for _, otherDir := range *otherDirList {
					absCheck, _ := filepath.Abs(otherDir)
					relCheck, _ := filepath.Rel(absCheck, absSource)
					if !strings.HasPrefix(relCheck, "..") {
						if relCheck != "." {
							// subdir
							coveringDir = &otherDir
							break
						}
						// dup... we only care if checkOtherDups is true
						if checkOtherDups {
							coveringDir = &otherDir
							break
						}
					}
				}
			}
			if coveringDir != nil {
				shared.LogInfof(
					reqLogger,
					cr,
					shared.EventReasonNoEvent,
					"skipping {%s} from %s persistDirs; dir {%s} covers it",
					sourceDir,
					sourceDesc,
					*coveringDir,
				)
				continue
			}
			// OK to add to the list.
			persistDirs = append(persistDirs, absSource)
		}
	}

	// Time to build the union of directory info to persist. This is not
	// quite the most efficient way to do it, but it's reasonable (for a
	// one-time-per-statefulset op and a small number of dirs) and not
	// too confusing to reason about its correctness and memory safety.

	// First let's take any of the default dirs that are not a subdirectory
	// of any other default or app dir, or a dup of a default dir.
	addToDirs(defaultPersistDirs, appPersistDirs, false, "default")

	// Now add any of the app dirs that are not a subdirectory or dup of any
	// other default or app dir.
	if appPersistDirs != nil {
		addToDirs(*appPersistDirs, &defaultPersistDirs, true, role.Name)
	}

	useServiceAccount := false
	if role.ServiceAccountName != "" {
		useServiceAccount = true
	}
	volumeMounts, volumes, volumesErr := generateVolumeMounts(
		cr,
		role,
		PvcNamePrefix,
		nativeSystemdSupport,
		persistDirs,
	)

	if volumesErr != nil {
		return nil, volumesErr
	}

	// check if BlockStorage field is present. If it is, create a volumeDevices field
	var volumeDevices []v1.VolumeDevice
	if role.BlockStorage != nil {

		numDevices := *role.BlockStorage.NumDevices

		for i := int32(0); i < numDevices; i++ {

			deviceID := strconv.FormatInt(int64(i), 10)
			devicePath := *role.BlockStorage.Path + deviceID
			deviceName := blockPvcNamePrefix + deviceID

			volumeDevice := v1.VolumeDevice{
				Name:       deviceName,
				DevicePath: devicePath,
			}
			volumeDevices = append(volumeDevices, volumeDevice)

		}

	}
	imageID, imageErr := catalog.ImageForRole(cr, role.Name)
	if imageErr != nil {
		return nil, imageErr
	}

	securityContext, securityErr := generateSecurityContext(cr)
	if securityErr != nil {
		return nil, securityErr
	}

	vct := getVolumeClaimTemplate(cr, role, PvcNamePrefix)

	sset := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			Kind:       "StatefulSet",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       cr.Namespace,
			OwnerReferences: shared.OwnerReferences(cr),
			Labels:          labels,
			Annotations:     annotations,
		},
		Spec: appsv1.StatefulSetSpec{
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Replicas:            &replicas,
			ServiceName:         cr.Status.ClusterService,
			Selector: &metav1.LabelSelector{
				MatchLabels: podLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: v1.PodSpec{
					AutomountServiceAccountToken: &useServiceAccount,
					InitContainers: getInitContainer(
						cr,
						role,
						PvcNamePrefix,
						imageID,
						persistDirs,
					),
					Affinity:           role.Affinity,
					ServiceAccountName: role.ServiceAccountName,
					Containers: []v1.Container{
						{
							Name:            AppContainerName,
							Image:           imageID,
							Resources:       role.Resources,
							Lifecycle:       &v1.Lifecycle{PostStart: &startupScript},
							Ports:           endpointPorts,
							VolumeMounts:    volumeMounts,
							VolumeDevices:   volumeDevices,
							SecurityContext: securityContext,
							Env:             chkModifyEnvVars(role, setupInfo),
							TTY:             hasTTY(cr, role.Name),
							Stdin:           hasSTDIN(cr, role.Name),
						},
					},
					Volumes: volumes,
				},
			},
			VolumeClaimTemplates: vct,
		},
	}

	namingScheme := *cr.Spec.NamingScheme
	if (roleStatus == nil) || (roleStatus.StatefulSet == "") {
		if namingScheme == v1beta1.CrNameRole {
			sset.ObjectMeta.GenerateName = MungObjectName(cr.Name + "-" + role.Name)
			sset.ObjectMeta.GenerateName += "-"
		} else if namingScheme == v1beta1.UID {
			sset.ObjectMeta.GenerateName = statefulSetNamePrefix
		}
	} else {
		sset.ObjectMeta.Name = roleStatus.StatefulSet
	}

	return sset, nil
}

// chkModifyEnvVars checks a role's resource requests. If an NVIDIA GPU resource
// has NOT been requested for the role, a work-around is added (as an environment
// variable), to avoid a GPU being surfaced anyway in a container related to
// the role. The PYTHONUSERBASE environment var will also be set to /usr/local
// if the role's useNewSetupLayout flag is true.
func chkModifyEnvVars(
	role *kdv1.Role,
	setupInfo *kdv1.SetupPackageInfo,
) (envVar []v1.EnvVar) {

	envVar = role.EnvVars

	// Handle PYTHONUSERBASE first.
	if setupInfo != nil {
		if setupInfo.UseNewSetupLayout {
			pythonUserBase := v1.EnvVar{
				Name:  "PYTHONUSERBASE",
				Value: shared.ConfigCliLoc,
				// ValueFrom not used
			}
			envVar = append(envVar, pythonUserBase)
		}
	}

	rsrcmap := role.Resources.Requests
	// return the role's environment variables unmodified, if an NVIDIA GPU is
	// indeed a resource requested for this role
	if quantity, found := rsrcmap[nvidiaGpuResourceName]; found == true && quantity.IsZero() != true {
		return envVar
	}

	// add an environment variable, as a work-around to ensure that an NVIDIA GPU is
	// not visible in a container (related to this role) for which an NVIDIA GPU resource
	// has not been requested (or the key for the NVIDIA GPU resource has been specified, but
	// with a quantity of zero)
	envVarToAdd := v1.EnvVar{
		Name:  nvidiaGpuVisWorkaroundEnvVarName,
		Value: nvidiaGpuVisWorkaroundEnvVarValue,
		// ValueFrom not used
	}
	envVar = append(envVar, envVarToAdd)
	return
}

// getInitContainer prepares the init container spec to be used with the
// given role (for initializing the directory content placed on shared
// persistent storage). The result will be empty if the role does not use
// shared persistent storage.
func getInitContainer(
	cr *kdv1.KubeDirectorCluster,
	role *kdv1.Role,
	pvcNamePrefix string,
	imageID string,
	persistDirs []string,
) (initContainer []v1.Container) {

	// We are depending on the default value of 0 here. Not setting it
	// explicitly because golint doesn't like that.
	var rootUID int64

	if role.Storage == nil {
		return
	}

	initVolumeMounts := generateInitVolumeMounts(pvcNamePrefix)
	initContainer = []v1.Container{
		{
			Args: []string{
				"-c",
				generateInitContainerLaunch(persistDirs),
			},
			Command: []string{
				"/bin/bash",
			},
			Image:     imageID,
			Name:      initContainerName,
			Resources: role.Resources,
			SecurityContext: &v1.SecurityContext{
				RunAsUser: &rootUID,
			},
			VolumeMounts: initVolumeMounts,
		},
	}
	return
}

// getVolumeClaimTemplate prepares the PVC templates to be used with the
// given role (for acquiring shared persistent storage). The result will be
// empty if the role does not use shared persistent storage. If the spec contains
// Storage field, a volume Volume Claim with Filesystem volume mode is created. If spec contains a BlockStorage field,
// BlockStorage field, a block Claim with Block volume mode is created.
func getVolumeClaimTemplate(
	cr *kdv1.KubeDirectorCluster,
	role *kdv1.Role,
	pvcNamePrefix string,
) (volTemplate []v1.PersistentVolumeClaim) {

	if role.Storage != nil {
		volSize, _ := resource.ParseQuantity(role.Storage.Size)
		volClaim := v1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: pvcNamePrefix,
			},
			Spec: v1.PersistentVolumeClaimSpec{
				AccessModes: []v1.PersistentVolumeAccessMode{
					v1.ReadWriteOnce,
				},
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{
						v1.ResourceStorage: volSize,
					},
				},
				StorageClassName: role.Storage.StorageClass,
			},
		}
		volTemplate = append(volTemplate, volClaim)
	}

	if role.BlockStorage != nil {

		block := v1.PersistentVolumeBlock

		blockVolSize, _ := resource.ParseQuantity(defaultBlockDeviceSize)

		if role.BlockStorage.Size != nil {
			blockVolSize, _ = resource.ParseQuantity(*role.BlockStorage.Size)
		}

		numDevices := *role.BlockStorage.NumDevices

		for i := int32(0); i < numDevices; i++ {

			deviceID := strconv.FormatInt(int64(i), 10)
			deviceName := blockPvcNamePrefix + deviceID

			blockClaim := v1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name: deviceName,
				},
				Spec: v1.PersistentVolumeClaimSpec{
					AccessModes: []v1.PersistentVolumeAccessMode{
						v1.ReadWriteOnce,
					},
					Resources: v1.ResourceRequirements{
						Requests: v1.ResourceList{
							v1.ResourceStorage: blockVolSize,
						},
					},
					StorageClassName: role.BlockStorage.StorageClass,

					VolumeMode: &block,
				},
			}

			volTemplate = append(volTemplate, blockClaim)
		}
	}
	return volTemplate
}

// getStartupScript composes the startup script used for each app container.
// Currently this adds the virtual cluster's DNS subdomain to the resolv.conf
// search list.
func getStartupScript(
	cr *kdv1.KubeDirectorCluster,
) v1.Handler {

	return v1.Handler{
		Exec: &v1.ExecAction{
			Command: []string{
				"/bin/bash",
				"-c",
				"exec 2>>/tmp/kd-postcluster.log; set -x;" +
					"Retries=60; while [[ $Retries && ! -s /etc/resolv.conf ]]; do " +
					"sleep 1; Retries=$(expr $Retries - 1); done; " +
					"sed \"s/^search \\([^ ]\\+\\)/search " +
					cr.Status.ClusterService +
					".\\1 \\1/\" /etc/resolv.conf > /tmp/resolv.conf.new && " +
					"cat /tmp/resolv.conf.new > /etc/resolv.conf;" +
					"rm -f /tmp/resolv.conf.new;" +
					"chmod 755 /run;" +
					"exit 0",
			},
		},
	}
}

// genrateRsyncInstalledCmd checks if the rsync command is available.
// If rsync is installed and all the options are available
// the RSYNC_CHECK_STATUS variable will be 0.
func genrateRsyncInstalledCmd() string {

	// Here we check two things:
	// 1) rsync is installed and available
	// 2) The options --log-file, --info=progress2 --relative -a -x are available.
	// Some of these options are not available in the first
	// versions of rsync.
	// The rsync-check-status-dummy.log file (dummy log file) is not used.
	// It is needed only to check that option --log-file is available
	cmd := "rsync --log-file=./rsync-check-status-dummy.log --info=progress2 --relative -ax --version; RSYNC_CHECK_STATUS=$?;"
	return cmd
}

// generateRsyncCmd generates command that will do copying with rsync
// The progress will be stored in a file.
func generateRsyncCmd(
	persistDirs []string,
) string {

	// The directory should be created in /mnt in advance,
	// otherwise the rsync log file will not be created
	createRsyncLogFileBaseDir := fmt.Sprintf("mkdir -p /mnt%s", filepath.Dir(kubedirectorInitLogs))

	rsyncCmd := fmt.Sprintf("%s; rsync --log-file=/mnt%s --info=progress2 --relative -ax %s /mnt > /mnt%s;",
		createRsyncLogFileBaseDir,
		kubedirectorInitLogs,
		strings.Join(persistDirs, " "),
		kubedirectorInitProgressBar)

	return rsyncCmd
}

// generateCpCmd generates command that will do copying with cp
// No way to display progress
func generateCpCmd(
	persistDirs []string,
) string {

	cpCmd := fmt.Sprintf("cp --parent -ax %s /mnt", strings.Join(persistDirs, " "))
	return cpCmd
}

// generateInitContainerLaunch generates the container entrypoint command for
// init containers. This command will populate the initial contents of the
// directories-to-be-persisted under the "/mnt" directory on the init
// container filesystem, then terminate the container.
func generateInitContainerLaunch(
	persistDirs []string,
) string {

	// To be safe in the case that this container is restarted by someone,
	// don't do this copy if the kubedirector.init file already exists in /etc.
	copyCondition := fmt.Sprintf("! [ -f /mnt%s ]", kubedirectorInit)

	// In order to perform copying rsync will be used.
	// It allows to report the progress that will be saved in a file.
	// Here we check if the rsync command is installed.
	rsyncInstalled := genrateRsyncInstalledCmd()

	// If the rsync command is not available the cp command will be used.
	fullCmd := fmt.Sprintf("%s %s && ( [ ${RSYNC_CHECK_STATUS} != 0 ] && (%s) || (%s)); touch /mnt%s;",
		rsyncInstalled,
		copyCondition,
		generateCpCmd(persistDirs),
		generateRsyncCmd(persistDirs),
		kubedirectorInit)

	return fullCmd
}

// generateSecretVolume generates VolumeMount and Volume
// object for mounting a secret into a container
func generateSecretVolume(
	secret *kdv1.KDSecret,
) ([]v1.VolumeMount, []v1.Volume) {

	if secret != nil {
		secretVolName := "secret-vol-" + secret.Name
		secretVolumeSource := v1.SecretVolumeSource{
			SecretName:  secret.Name,
			DefaultMode: secret.DefaultMode,
		}
		return []v1.VolumeMount{
				v1.VolumeMount{
					Name:      secretVolName,
					MountPath: secret.MountPath,
					ReadOnly:  secret.ReadOnly,
				},
			}, []v1.Volume{
				v1.Volume{
					Name: secretVolName,
					VolumeSource: v1.VolumeSource{
						Secret: &secretVolumeSource,
					},
				},
			}
	}
	return []v1.VolumeMount{}, []v1.Volume{}

}

// generateVolumeProjectionMounts generates VolumeMount and Volume
// object for mounting volumeProjections
func generateVolumeProjectionMounts(
	volIndex int,
	projectedVol *kdv1.VolumeProjections,
) ([]v1.VolumeMount, []v1.Volume) {

	volName := "projected-vol-" + strconv.Itoa(volIndex)
	volSource := v1.PersistentVolumeClaimVolumeSource{
		ClaimName: projectedVol.PvcName,
		ReadOnly:  projectedVol.ReadOnly,
	}
	return []v1.VolumeMount{
			v1.VolumeMount{
				Name:      volName,
				MountPath: projectedVol.MountPath,
				ReadOnly:  projectedVol.ReadOnly,
			},
		}, []v1.Volume{
			v1.Volume{
				Name: volName,
				VolumeSource: v1.VolumeSource{
					PersistentVolumeClaim: &volSource,
				},
			},
		}
	return []v1.VolumeMount{}, []v1.Volume{}

}

// generateVolumeMounts generates all of an app container's volume and mount
// specs for persistent storage, tmpfs and systemctl support that are
// appropriate for members of the given role. For systemctl support,
// nativeSystemdSupport flag is examined along with the app requirement.
// Additionally generate volume mount spec if a role has
// requested for volume projections.
func generateVolumeMounts(
	cr *kdv1.KubeDirectorCluster,
	role *kdv1.Role,
	pvcNamePrefix string,
	nativeSystemdSupport bool,
	persistDirs []string,
) ([]v1.VolumeMount, []v1.Volume, error) {
	var volumeMounts []v1.VolumeMount
	var volumes []v1.Volume

	if role.Storage != nil {
		volumeMounts = generateClaimMounts(pvcNamePrefix, persistDirs)
	}

	tmpfsVolMnts, tmpfsVols := generateTmpfsSupport(cr)
	volumeMounts = append(volumeMounts, tmpfsVolMnts...)
	volumes = append(volumes, tmpfsVols...)

	// Generate secret volumes (if needed)
	secretVolMnts, secretVols := generateSecretVolume(role.Secret)
	volumeMounts = append(volumeMounts, secretVolMnts...)
	volumes = append(volumes, secretVols...)

	// Generate volume projections (if any)
	numVolumes := len(role.VolumeProjections)
	for i := 0; i < numVolumes; i++ {
		projectedVol := role.VolumeProjections[i]
		volProjectionMnts, volProjections := generateVolumeProjectionMounts(i, &projectedVol)

		volumeMounts = append(volumeMounts, volProjectionMnts...)
		volumes = append(volumes, volProjections...)
	}

	isSystemdReqd, err := catalog.SystemdRequired(cr)

	if err != nil {
		return volumeMounts, volumes, err
	}

	if isSystemdReqd && !nativeSystemdSupport {
		cgroupVolMnts, cgroupVols := generateSystemdSupport(cr)
		volumeMounts = append(volumeMounts, cgroupVolMnts...)
		volumes = append(volumes, cgroupVols...)
	}

	return volumeMounts, volumes, nil
}

// generateClaimMounts creates the mount specs for all directories that are
// to be mounted from a persistent volume by an app container.
func generateClaimMounts(
	pvcNamePrefix string,
	persistDirs []string,
) []v1.VolumeMount {

	var volumeMounts []v1.VolumeMount
	for _, folder := range persistDirs {
		volumeMount := v1.VolumeMount{
			MountPath: folder,
			Name:      pvcNamePrefix,
			ReadOnly:  false,
			SubPath:   folder[1:],
		}
		volumeMounts = append(volumeMounts, volumeMount)
	}
	return volumeMounts
}

// generateInitVolumeMounts creates the spec for mounting a persistent volume
// into an init container.
func generateInitVolumeMounts(
	pvcNamePrefix string,
) []v1.VolumeMount {

	return []v1.VolumeMount{
		v1.VolumeMount{
			MountPath: "/mnt",
			Name:      pvcNamePrefix,
			ReadOnly:  false,
		},
	}
}

// generateSystemdSupport creates the volume and mount specs necessary for
// supporting the use of systemd within an app container by mounting
// appropriate /sys/fs/cgroup directories from the host.
func generateSystemdSupport(
	cr *kdv1.KubeDirectorCluster,
) ([]v1.VolumeMount, []v1.Volume) {

	cgroupFsName := "cgroupfs"
	systemdFsName := "systemd"
	volumeMounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      cgroupFsName,
			MountPath: cgroupFSVolume,
			ReadOnly:  true,
		},
		v1.VolumeMount{
			Name:      systemdFsName,
			MountPath: systemdFSVolume,
		},
	}
	volumes := []v1.Volume{
		v1.Volume{
			Name: cgroupFsName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: cgroupFSVolume,
				},
			},
		},
		v1.Volume{
			Name: systemdFsName,
			VolumeSource: v1.VolumeSource{
				HostPath: &v1.HostPathVolumeSource{
					Path: systemdFSVolume,
				},
			},
		},
	}
	return volumeMounts, volumes
}

// generateTmpfsSupport creates the volume and mount specs necessary for
// backing an app container's /tmp and /run directories with a ramdisk. Limit
// the size of the ramdisk to tmpFsVolSize.
func generateTmpfsSupport(
	cr *kdv1.KubeDirectorCluster,
) ([]v1.VolumeMount, []v1.Volume) {

	volumeMounts := []v1.VolumeMount{
		v1.VolumeMount{
			Name:      "tmpfs-tmp",
			MountPath: "/tmp",
		},
		v1.VolumeMount{
			Name:      "tmpfs-run",
			MountPath: "/run",
		},
		v1.VolumeMount{
			Name:      "tmpfs-run-lock",
			MountPath: "/run/lock",
		},
	}
	maxTmpSize, _ := resource.ParseQuantity(tmpFSVolSize)
	volumes := []v1.Volume{
		v1.Volume{
			Name: "tmpfs-tmp",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{
					Medium:    "Memory",
					SizeLimit: &maxTmpSize,
				},
			},
		},
		v1.Volume{
			Name: "tmpfs-run",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{
					Medium:    "Memory",
					SizeLimit: &maxTmpSize,
				},
			},
		},
		v1.Volume{
			Name: "tmpfs-run-lock",
			VolumeSource: v1.VolumeSource{
				EmptyDir: &v1.EmptyDirVolumeSource{
					Medium:    "Memory",
					SizeLimit: &maxTmpSize,
				},
			},
		},
	}
	return volumeMounts, volumes
}

// generateSecurityContext creates security context with Add Capabilities property
// based on app's capability list. If app doesn't require additional capabilities
// return nil
func generateSecurityContext(
	cr *kdv1.KubeDirectorCluster,
) (*v1.SecurityContext, error) {

	appCapabilities, err := catalog.AppCapabilities(cr)
	if err != nil {
		return nil, err
	}

	if len(appCapabilities) == 0 {
		return nil, err
	}

	return &v1.SecurityContext{
		Capabilities: &v1.Capabilities{
			Add: appCapabilities,
		},
	}, nil
}

// hasSTDIN is a utility function to find out
// if STDIN was requested by the KubeDirectorApp
// default is False if left blank by the App
func hasSTDIN(
	cr *kdv1.KubeDirectorCluster,
	role string,
) bool {

	containerSpec, _ := catalog.RoleContainerSpecs(cr, role)
	if containerSpec == nil {
		return false
	}

	return containerSpec.Stdin
}

// hasTTY is a utility function to find out
// if TTY was requested by the KubeDirectorApp
// default is False if left blank by the App
func hasTTY(
	cr *kdv1.KubeDirectorCluster,
	role string,
) bool {

	containerSpec, _ := catalog.RoleContainerSpecs(cr, role)
	if containerSpec == nil {
		return false
	}

	return containerSpec.Tty
}
