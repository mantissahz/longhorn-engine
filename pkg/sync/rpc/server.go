package rpc

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/longhorn/backupstore"
	butil "github.com/longhorn/backupstore/util"
	"github.com/longhorn/sparse-tools/sparse"
	sparserest "github.com/longhorn/sparse-tools/sparse/rest"
	"github.com/longhorn/types/pkg/generated/enginerpc"
	"github.com/moby/moby/pkg/reexec"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"

	lhio "github.com/longhorn/go-common-libs/io"

	"github.com/longhorn/longhorn-engine/pkg/backup"
	"github.com/longhorn/longhorn-engine/pkg/interceptor"
	"github.com/longhorn/longhorn-engine/pkg/replica"
	replicaclient "github.com/longhorn/longhorn-engine/pkg/replica/client"
	"github.com/longhorn/longhorn-engine/pkg/types"
	"github.com/longhorn/longhorn-engine/pkg/util"
	diskutil "github.com/longhorn/longhorn-engine/pkg/util/disk"
)

/*
 * Lock sequence
 * 1. SyncAgentServer
 * 2. BackupList, RestoreInfo or PurgeStatus (cannot be hold at the same time)
 */

const (
	PeriodicRefreshIntervalInSeconds = 2

	GRPCServiceCommonTimeout = 3 * time.Minute
)

type SyncAgentServer struct {
	enginerpc.UnimplementedSyncAgentServiceServer
	sync.RWMutex

	currentPort     int
	startPort       int
	endPort         int
	processesByPort map[int]string
	isPurging       bool
	isRestoring     bool
	isRebuilding    bool
	isCloning       bool
	replicaAddress  string
	volumeName      string
	instanceName    string

	BackupList       *BackupList
	SnapshotHashList *SnapshotHashList
	RestoreInfo      *replica.RestoreStatus
	PurgeStatus      *PurgeStatus
	RebuildStatus    *RebuildStatus
	CloneStatus      *CloneStatus
}

type PurgeStatus struct {
	sync.RWMutex
	Error    string
	Progress int
	State    types.ProcessState

	processed int
	total     int
}

func (ps *PurgeStatus) UpdateFileHandlingProgress(progress int, done bool, err error) {
	ps.Lock()
	defer ps.Unlock()

	// Avoid possible division by zero, also total 0 means nothing to be done
	if ps.total == 0 {
		ps.Progress = 100
	} else {
		ps.Progress = int((float32(ps.processed)/float32(ps.total) + float32(progress)/(float32(ps.total)*100)) * 100)
	}
}

type RebuildStatus struct {
	sync.RWMutex
	Error              string
	Progress           int
	State              types.ProcessState
	FromReplicaAddress string

	processedSize int64
	totalSize     int64
}

func (rs *RebuildStatus) UpdateSyncFileProgress(size int64) {
	rs.Lock()
	defer rs.Unlock()

	rs.processedSize = rs.processedSize + size
	rs.Progress = int((float32(rs.processedSize) / float32(rs.totalSize)) * 100)
}

type CloneStatus struct {
	sync.RWMutex
	Error              string
	Progress           int
	State              types.ProcessState
	FromReplicaAddress string
	SnapshotName       string

	processedSize int64
	totalSize     int64
}

func (cs *CloneStatus) UpdateSyncFileProgress(size int64) {
	cs.Lock()
	defer cs.Unlock()

	cs.processedSize = cs.processedSize + size
	cs.Progress = int((float32(cs.processedSize) / float32(cs.totalSize)) * 100)
}

func NewSyncAgentServer(startPort, endPort int, replicaAddress, volumeName, instanceName string) *grpc.Server {
	sas := &SyncAgentServer{
		currentPort:     startPort,
		startPort:       startPort,
		endPort:         endPort,
		processesByPort: map[int]string{},
		replicaAddress:  replicaAddress,
		volumeName:      volumeName,
		instanceName:    instanceName,

		BackupList:       &BackupList{},
		SnapshotHashList: &SnapshotHashList{},
		RestoreInfo:      &replica.RestoreStatus{},
		PurgeStatus:      &PurgeStatus{},
		RebuildStatus:    &RebuildStatus{},
		CloneStatus:      &CloneStatus{},
	}
	server := grpc.NewServer(interceptor.WithIdentityValidationReplicaServerInterceptor(volumeName, instanceName))
	enginerpc.RegisterSyncAgentServiceServer(server, sas)
	reflection.Register(server)
	return server
}

func (s *SyncAgentServer) nextPort(processName string) (int, error) {
	s.Lock()
	defer s.Unlock()

	// Must be called with s.Lock() obtained
	for i := 0; i < (s.endPort - s.startPort + 1); i++ {
		port := s.currentPort
		s.currentPort++
		if s.currentPort > s.endPort {
			s.currentPort = s.startPort
		}

		if _, ok := s.processesByPort[port]; ok {
			continue
		}

		s.processesByPort[port] = processName

		return port, nil
	}

	return 0, errors.New("out of ports")
}

func (s *SyncAgentServer) IsRestoring() bool {
	s.RLock()
	defer s.RUnlock()
	return s.isRestoring
}

func (s *SyncAgentServer) StartRestore(backupURL, requestedBackupName, snapshotDiskName string, concurrentLimit int) (err error) {
	s.Lock()
	defer s.Unlock()

	defer func() {
		if err == nil {
			s.isRestoring = true
		}
	}()
	if s.isRestoring {
		return fmt.Errorf("cannot initiate backup restore as there is one already in progress")
	}

	if s.RestoreInfo == nil {
		return fmt.Errorf("BUG: the restore status is not initialized in the sync agent server")
	}

	restoreStatus := s.RestoreInfo.DeepCopy()
	if restoreStatus.State == replica.ProgressStateError {
		return fmt.Errorf("cannot start backup restore of the previous restore fails")
	}
	if restoreStatus.LastRestored == requestedBackupName {
		return fmt.Errorf("already restored backup %v", requestedBackupName)
	}

	// Initialize `s.RestoreInfo`
	// First restore request. It must be a normal full restore.
	if restoreStatus.LastRestored == "" && restoreStatus.State == "" {
		s.RestoreInfo = replica.NewRestore(snapshotDiskName, s.replicaAddress, backupURL, requestedBackupName)
	} else {
		var toFileName string
		validLastRestoredBackup := s.canDoIncrementalRestore(restoreStatus, backupURL, requestedBackupName)
		if validLastRestoredBackup {
			toFileName = diskutil.GenerateDeltaFileName(restoreStatus.LastRestored)
		} else {
			toFileName = diskutil.GenerateSnapTempFileName(snapshotDiskName)
		}
		s.RestoreInfo.StartNewRestore(backupURL, requestedBackupName, toFileName, snapshotDiskName, validLastRestoredBackup)
	}

	// Initiate restore
	newRestoreStatus := s.RestoreInfo.DeepCopy()
	defer func() {
		if err != nil {
			logrus.Warn("Failed to initiate the backup restore, will do revert and clean up then.")
			if newRestoreStatus.ToFileName != newRestoreStatus.SnapshotDiskName {
				if errRemove := os.Remove(newRestoreStatus.ToFileName); errRemove != nil {
					logrus.WithError(errRemove).Errorf("Failed to remove file %v", newRestoreStatus.ToFileName)
				}
			}
			s.RestoreInfo.Revert(restoreStatus)
		}
	}()

	if newRestoreStatus.LastRestored == "" {
		if err := backup.DoBackupRestore(backupURL, newRestoreStatus.ToFileName, concurrentLimit, s.RestoreInfo); err != nil {
			return errors.Wrapf(err, "error initiating full backup restore")
		}
		logrus.Infof("Successfully initiated full restore for %v to [%v]", backupURL, newRestoreStatus.ToFileName)
	} else {
		if err := backup.DoBackupRestoreIncrementally(backupURL, newRestoreStatus.ToFileName, newRestoreStatus.LastRestored, concurrentLimit, s.RestoreInfo); err != nil {
			return errors.Wrapf(err, "error initiating incremental backup restore")
		}
		logrus.Infof("Successfully initiated incremental restore for %v to [%v]", backupURL, newRestoreStatus.ToFileName)
	}

	return nil
}

func (s *SyncAgentServer) canDoIncrementalRestore(restoreStatus *replica.RestoreStatus, backupURL, requestedBackupName string) bool {
	if restoreStatus.LastRestored == "" {
		logrus.Warnf("There is a restore record in the server but last restored backup is empty with restore state is %v, will do full restore instead", restoreStatus.State)
		return false
	}
	if _, err := backupstore.InspectBackup(strings.Replace(backupURL, requestedBackupName, restoreStatus.LastRestored, 1)); err != nil {
		logrus.WithError(err).Warnf("The last restored backup %v becomes invalid for incremental restore, will do full restore instead", restoreStatus.LastRestored)
		return false
	}
	return true
}

func (s *SyncAgentServer) FinishRestore(restoreErr error) (err error) {
	s.Lock()
	defer s.Unlock()

	defer func() {
		if s.RestoreInfo != nil {
			if restoreErr != nil {
				s.RestoreInfo.UpdateRestoreStatus(s.RestoreInfo.ToFileName, 0, restoreErr)
			} else {
				s.RestoreInfo.FinishRestore()
			}
		}
	}()

	if !s.isRestoring {
		err = fmt.Errorf("BUG: volume is not restoring")
		if restoreErr != nil {
			restoreErr = types.CombineErrors(err, restoreErr)
		} else {
			restoreErr = err
		}
		return err
	}

	s.isRestoring = false

	return nil
}

func (s *SyncAgentServer) Reset(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	s.Lock()
	defer s.Unlock()
	if s.isRestoring {
		logrus.Error("Replica is currently restoring, cannot reset")
		return nil, fmt.Errorf("replica is currently restoring, cannot reset")
	}
	s.isRestoring = false
	s.BackupList = &BackupList{}
	s.SnapshotHashList = &SnapshotHashList{}
	s.RestoreInfo = &replica.RestoreStatus{}
	s.RebuildStatus = &RebuildStatus{}
	s.PurgeStatus = &PurgeStatus{}
	s.CloneStatus = &CloneStatus{}
	return &emptypb.Empty{}, nil
}

func (*SyncAgentServer) FileRemove(ctx context.Context, req *enginerpc.FileRemoveRequest) (*emptypb.Empty, error) {
	logrus.Infof("Running rm %v", req.FileName)

	if err := os.Remove(req.FileName); err != nil {
		logrus.Infof("Error running %s %v: %v", "rm", req.FileName, err)
		return nil, err
	}

	logrus.Infof("Done running %s %v", "rm", req.FileName)
	return &emptypb.Empty{}, nil
}

func (*SyncAgentServer) FileRename(ctx context.Context, req *enginerpc.FileRenameRequest) (*emptypb.Empty, error) {
	logrus.Infof("Running rename file from %v to %v", req.OldFileName, req.NewFileName)

	if err := os.Rename(req.OldFileName, req.NewFileName); err != nil {
		logrus.Infof("Error running %s from %v to %v: %v", "rename", req.OldFileName, req.NewFileName, err)
		return nil, err
	}

	logrus.Infof("Done running %s from %v to %v", "rename", req.OldFileName, req.NewFileName)
	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) FileSend(ctx context.Context, req *enginerpc.FileSendRequest) (*emptypb.Empty, error) {
	address := net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))
	directIO := filepath.Ext(strings.TrimSpace(req.FromFileName)) != ".meta"

	logrus.Infof("Syncing file %v to %v", req.FromFileName, address)
	if err := sparse.SyncFile(req.FromFileName, address, int(req.FileSyncHttpClientTimeout), directIO, req.FastSync); err != nil {
		return nil, err
	}
	logrus.Infof("Done syncing file %v to %v", req.FromFileName, address)

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) VolumeExport(ctx context.Context, req *enginerpc.VolumeExportRequest) (*emptypb.Empty, error) {
	remoteAddress := net.JoinHostPort(req.Host, strconv.Itoa(int(req.Port)))

	var err error
	defer func() {
		if err != nil {
			logrus.WithError(err).Warnf("Failed to export snapshot %v to %v", req.SnapshotFileName, remoteAddress)
		}
	}()

	logrus.Infof("Exporting snapshot %v to %v", req.SnapshotFileName, remoteAddress)
	dir, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get working directory")
	}
	r, err := replica.OpenSnapshot(dir, req.SnapshotFileName)
	if err != nil {
		return nil, err
	}
	defer r.CloseWithoutWritingMetaData()
	if err := r.Preload(req.ExportBackingImageIfExist); err != nil {
		return nil, err
	}
	if err := sparse.SyncContent(req.SnapshotFileName, r, r.Info().Size, remoteAddress, int(req.FileSyncHttpClientTimeout), true, false); err != nil {
		return nil, err
	}

	logrus.Infof("Done exporting snapshot %v to %v", req.SnapshotFileName, remoteAddress)

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) ReceiverLaunch(ctx context.Context, req *enginerpc.ReceiverLaunchRequest) (*enginerpc.ReceiverLaunchResponse, error) {
	port, err := s.launchReceiver("ReceiverLaunch", req.ToFileName, &sparserest.SyncFileStub{})
	if err != nil {
		return nil, err
	}
	logrus.Infof("Launching receiver for file %v", req.ToFileName)

	return &enginerpc.ReceiverLaunchResponse{Port: int32(port)}, nil
}

func (s *SyncAgentServer) launchReceiver(processName, toFileName string, ops sparserest.SyncFileOperations) (int, error) {
	port, err := s.nextPort(processName)
	if err != nil {
		return 0, err
	}

	go func() {
		defer func() {
			s.Lock()
			delete(s.processesByPort, port)
			s.Unlock()
		}()

		logrus.Infof("Running ssync server for file %v at port %v", toFileName, port)
		if err = sparserest.Server(context.Background(), strconv.Itoa(port), toFileName, ops); err != nil && err != http.ErrServerClosed {
			logrus.WithError(err).Error("Error running ssync server")
			return
		}
		logrus.Infof("Done running ssync server for file %v at port %v", toFileName, port)
	}()

	return port, nil
}

func (s *SyncAgentServer) FilesSync(ctx context.Context, req *enginerpc.FilesSyncRequest) (res *emptypb.Empty, err error) {
	if err := s.PrepareRebuild(req.SyncFileInfoList, req.FromAddress); err != nil {
		return nil, err
	}

	defer func() {
		s.RebuildStatus.Lock()
		if err != nil {
			s.RebuildStatus.Error = err.Error()
			s.RebuildStatus.State = types.ProcessStateError
			logrus.WithError(err).Error("Sync agent gRPC server failed to rebuild replica/sync files")
		} else {
			s.RebuildStatus.Progress = 100
			s.RebuildStatus.State = types.ProcessStateComplete
			logrus.Infof("Sync agent gRPC server finished rebuilding replica/sync files for replica %v", req.ToHost)
		}
		s.RebuildStatus.Unlock()

		// We must be careful not to return a nil error if we failed previously but finished successfully.
		if finishErr := s.FinishRebuild(); finishErr != nil {
			logrus.WithError(finishErr).Error("Could not finish rebuilding")
			finishErr = errors.Wrap(finishErr, "could not finish rebuilding")
			if err == nil {
				err = finishErr
			} else {
				err = errors.Wrap(err, finishErr.Error())
			}
		}
	}()

	if req.LocalSync != nil {
		err := s.fileSyncLocal(ctx, req)
		if err == nil {
			return &emptypb.Empty{}, nil
		}

		logrus.WithError(err).Warn("Falling back to remote sync")
	}

	return &emptypb.Empty{}, s.fileSyncRemote(ctx, req)
}

func (s *SyncAgentServer) fileSyncLocal(ctx context.Context, req *enginerpc.FilesSyncRequest) error {
	var targetPaths []string
	var err error

	log := logrus.WithFields(logrus.Fields{
		"sourcePath": req.LocalSync.SourcePath,
		"targetPath": req.LocalSync.TargetPath,
	})

	log.Info("Syncing files locally")

	// Defer function to handle cleanup of files if an error occurs
	defer func() {
		if err == nil {
			log.Info("Done syncing files locally")
		} else {
			log.WithError(err).Warn("Failed to sync files locally, reverting changes")

			for _, targetPath := range targetPaths {
				if removeErr := os.Remove(targetPath); removeErr != nil && removeErr != os.ErrNotExist {
					log.WithError(removeErr).Warnf("Failed to remove file %v", targetPath)
				}
			}
		}
	}()

	for _, info := range req.SyncFileInfoList {
		sourcePath := filepath.Join("/host", req.LocalSync.SourcePath, info.FromFileName)
		targetPath := filepath.Join("/host", req.LocalSync.TargetPath, info.ToFileName)
		targetPaths = append(targetPaths, targetPath)

		log.Tracef("Copying file %v to %v", sourcePath, targetPath)

		err = sparse.SyncLocalFile(sourcePath, targetPath)
		if err != nil {
			return err
		}

		err := lhio.CheckIsFileSizeSame(sourcePath, targetPath)
		if err != nil {
			return errors.Wrapf(err, "failed to check file size for file %v", sourcePath)
		}
	}

	return nil
}

func (s *SyncAgentServer) fileSyncRemote(ctx context.Context, req *enginerpc.FilesSyncRequest) error {
	// We generally don't know the from replica's instanceName since it is arbitrarily chosen from candidate addresses
	// stored in the controller. Don't modify FilesSyncRequest to contain it, and create a client without it.
	fromClient, err := replicaclient.NewReplicaClient(req.FromAddress, s.volumeName, "")
	if err != nil {
		return err
	}
	defer func() {
		if errClose := fromClient.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for replica address %v", req.FromAddress)
		}
	}()

	var ops sparserest.SyncFileOperations
	fileStub := &sparserest.SyncFileStub{}

	for _, info := range req.SyncFileInfoList {
		// Do not count size for disk meta file or empty disk file.
		if info.ActualSize == 0 {
			ops = fileStub
		} else {
			ops = s.RebuildStatus
		}

		port, err := s.launchReceiver("FilesSync", info.ToFileName, ops)
		if err != nil {
			return errors.Wrapf(err, "failed to launch receiver for file %v", info.ToFileName)
		}
		if err := fromClient.SendFile(info.FromFileName, req.ToHost, int32(port), int(req.FileSyncHttpClientTimeout), req.FastSync, req.GrpcTimeoutSeconds); err != nil {
			return errors.Wrapf(err, "replica %v failed to send file %v to %v:%v", req.FromAddress, info.ToFileName, req.ToHost, port)
		}
	}
	return nil
}

func (s *SyncAgentServer) PrepareRebuild(list []*enginerpc.SyncFileInfo, fromReplicaAddress string) error {
	s.Lock()
	defer s.Unlock()

	if s.isPurging {
		return fmt.Errorf("replica is purging snapshots")
	}
	if s.isRebuilding {
		return fmt.Errorf("replica is already rebuilding")
	}

	s.isRebuilding = true

	s.RebuildStatus.Lock()
	s.RebuildStatus.FromReplicaAddress = fromReplicaAddress
	s.RebuildStatus.Error = ""
	s.RebuildStatus.State = types.ProcessStateInProgress
	// avoid possible division by zero
	s.RebuildStatus.processedSize = 1
	s.RebuildStatus.totalSize = 1
	for _, info := range list {
		s.RebuildStatus.totalSize += info.ActualSize
	}
	s.RebuildStatus.Progress = int((float32(s.RebuildStatus.processedSize) / float32(s.RebuildStatus.totalSize)) * 100)
	s.RebuildStatus.Unlock()

	return nil
}

func (s *SyncAgentServer) FinishRebuild() error {
	s.Lock()
	defer s.Unlock()

	if !s.isRebuilding {
		return fmt.Errorf("BUG: replica is not rebuilding")
	}

	s.isRebuilding = false
	return nil
}

func (s *SyncAgentServer) IsRebuilding() bool {
	s.RLock()
	defer s.RUnlock()

	return s.isRebuilding
}

func (s *SyncAgentServer) ReplicaRebuildStatus(ctx context.Context, req *emptypb.Empty) (*enginerpc.ReplicaRebuildStatusResponse, error) {
	isRebuilding := s.IsRebuilding()

	s.RebuildStatus.RLock()
	defer s.RebuildStatus.RUnlock()
	return &enginerpc.ReplicaRebuildStatusResponse{
		IsRebuilding:       isRebuilding,
		Error:              s.RebuildStatus.Error,
		Progress:           int32(s.RebuildStatus.Progress),
		State:              string(s.RebuildStatus.State),
		FromReplicaAddress: s.RebuildStatus.FromReplicaAddress,
	}, nil
}

func (s *SyncAgentServer) SnapshotClone(ctx context.Context, req *enginerpc.SnapshotCloneRequest) (res *emptypb.Empty, err error) {
	// We generally don't know the from replica's instanceName since it is arbitrarily chosen from candidate addresses
	// stored in the controller. Do don't modify SnapshotCloneRequest to contain it, and create a client without it.
	fromClient, err := replicaclient.NewReplicaClient(req.FromAddress, req.FromVolumeName, "")
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := fromClient.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", req.FromAddress)
		}
	}()

	sourceReplica, err := fromClient.GetReplica()
	if err != nil {
		return nil, err
	}
	if _, ok := sourceReplica.Disks[diskutil.GenerateSnapshotDiskName(req.SnapshotFileName)]; !ok {
		return nil, fmt.Errorf("cannot find snapshot %v in the source replica %v", req.SnapshotFileName, req.FromAddress)
	}
	snapshotSize, err := strconv.ParseInt(sourceReplica.Size, 10, 64)
	if err != nil {
		return nil, err
	}

	defer func() {
		s.CloneStatus.Lock()
		if err != nil {
			s.CloneStatus.Error = err.Error()
			s.CloneStatus.State = types.ProcessStateError
			logrus.WithError(err).Errorf("Sync agent gRPC server failed to clone snapshot %v from replica %v to replica %v", req.SnapshotFileName, req.FromAddress, req.ToHost)
		} else {
			s.CloneStatus.Progress = 100
			s.CloneStatus.State = types.ProcessStateComplete
			logrus.Infof("Sync agent gRPC server finished cloning snapshot %v from replica %v to replica %v", req.SnapshotFileName, req.FromAddress, req.ToHost)
		}
		s.CloneStatus.Unlock()
		if err = s.FinishClone(); err != nil {
			logrus.WithError(err).Error("Could not finish cloning")
		}
	}()

	if err := s.prepareClone(req.FromAddress, req.SnapshotFileName, snapshotSize); err != nil {
		return nil, err
	}

	if err := s.startCloning(req, fromClient); err != nil {
		return nil, err
	}

	if err := s.postCloning(); err != nil {
		return nil, err
	}

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) prepareClone(fromReplicaAddress, snapshotName string, snapshotSize int64) error {
	s.Lock()
	defer s.Unlock()
	cloneStatus := s.CloneStatus
	if s.isCloning {
		return fmt.Errorf("replica is cloning snapshot %v from replica address %v", cloneStatus.SnapshotName, cloneStatus.FromReplicaAddress)
	}
	s.isCloning = true

	cloneStatus.Lock()
	defer cloneStatus.Unlock()
	cloneStatus.FromReplicaAddress = fromReplicaAddress
	cloneStatus.SnapshotName = snapshotName
	cloneStatus.Error = ""
	cloneStatus.State = types.ProcessStateInProgress
	cloneStatus.totalSize = snapshotSize
	// avoid possible division by zero
	if cloneStatus.totalSize == 0 {
		cloneStatus.totalSize = 1
	}
	cloneStatus.Progress = int((float32(cloneStatus.processedSize) / float32(cloneStatus.totalSize)) * 100)

	return nil
}

func (s *SyncAgentServer) startCloning(req *enginerpc.SnapshotCloneRequest, fromReplicaClient *replicaclient.ReplicaClient) error {
	snapshotDiskName := diskutil.GenerateSnapshotDiskName(s.CloneStatus.SnapshotName)
	port, err := s.launchReceiver("SnapshotClone", snapshotDiskName, s.CloneStatus)
	if err != nil {
		return errors.Wrapf(err, "failed to launch receiver for snapshot %v", req.SnapshotFileName)
	}

	if err := fromReplicaClient.ExportVolume(req.SnapshotFileName, util.GetGRPCAddress(req.ToHost), int32(port), false, int(req.FileSyncHttpClientTimeout)); err != nil {
		return errors.Wrapf(err, "failed to export snapshot %v from replica %v to %v:%v", req.SnapshotFileName, req.FromAddress, req.ToHost, port)
	}

	return nil
}

func (s *SyncAgentServer) postCloning() error {
	snapshotDiskName := diskutil.GenerateSnapshotDiskName(s.CloneStatus.SnapshotName)
	if err := backup.CreateNewSnapshotMetafile(snapshotDiskName + ".meta"); err != nil {
		return errors.Wrapf(err, "failed creating meta snapshot file")
	}

	// Reload the replica so that the snapshot file can be loaded in the replica disk chain
	if err := s.reloadReplica(); err != nil {
		err = errors.Wrapf(err, "failed to reload replica %v during cloning snapshot %v", s.replicaAddress, snapshotDiskName)
		logrus.Error(err)
		return err
	}

	if err := s.replicaRevert(snapshotDiskName, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return errors.Wrapf(err, "error on reverting to %v on %v", snapshotDiskName, s.replicaAddress)
	}
	logrus.Infof("Reverting to snapshot %s on %s successful", snapshotDiskName, s.replicaAddress)
	return nil
}

func (s *SyncAgentServer) FinishClone() error {
	s.Lock()
	defer s.Unlock()

	if !s.isCloning {
		return fmt.Errorf("BUG: replica is not cloning")
	}

	s.isCloning = false
	return nil
}

func (s *SyncAgentServer) IsCloning() bool {
	s.RLock()
	defer s.RUnlock()

	return s.isCloning
}

func (s *SyncAgentServer) SnapshotCloneStatus(ctx context.Context, req *emptypb.Empty) (*enginerpc.SnapshotCloneStatusResponse, error) {
	isCloning := s.IsCloning()

	s.CloneStatus.RLock()
	defer s.CloneStatus.RUnlock()
	return &enginerpc.SnapshotCloneStatusResponse{
		IsCloning:          isCloning,
		Error:              s.CloneStatus.Error,
		Progress:           int32(s.CloneStatus.Progress),
		State:              string(s.CloneStatus.State),
		FromReplicaAddress: s.CloneStatus.FromReplicaAddress,
		SnapshotName:       s.CloneStatus.SnapshotName,
	}, nil
}

func (s *SyncAgentServer) BackupCreate(ctx context.Context, req *enginerpc.BackupCreateRequest) (*enginerpc.BackupCreateResponse, error) {
	backupType, err := butil.CheckBackupType(req.BackupTarget)
	if err != nil {
		return nil, err
	}

	if err := butil.SetupCredential(backupType, req.Credential); err != nil {
		return nil, err
	}

	// Mounting NFS is part of the backup initialization, and at this stage, the backup status is not
	// created and is not added to the BackupList.
	//
	// In soft mode, a stuck operation is only retried twice. To prevent the backup monitor being trapped in
	// an infinite backup status polling loop, the sync agent server needs to record the backup status before executing
	// the backup. After the retries fail, the state transitions to error. The backup monitor is then aware of the error
	// and marked the backup failed, and won't poll the backup status infinitely.
	backupStatus, backupConfig, err := backup.DoBackupInit(&backup.CreateBackupParameters{
		BackupName:           req.BackupName,
		VolumeName:           req.VolumeName,
		SnapshotName:         req.SnapshotFileName,
		DestURL:              req.BackupTarget,
		BackingImageName:     req.BackingImageName,
		BackingImageChecksum: req.BackingImageChecksum,
		CompressionMethod:    req.CompressionMethod,
		ConcurrentLimit:      req.ConcurrentLimit,
		StorageClassName:     req.StorageClassName,
		Labels:               req.Labels,
		Parameters:           req.Parameters,
	})
	if err != nil {
		logrus.WithError(err).Errorf("Failed to initialize backup %v", req.BackupName)
		return nil, err
	}

	if err := s.BackupList.BackupAdd(backupStatus.Name, backupStatus); err != nil {
		return nil, errors.Wrapf(err, "failed to add the backup object %v", backupStatus.Name)
	}

	err = backup.DoBackupCreate(backupStatus, backupConfig)
	if err != nil {
		logrus.WithError(err).Errorf("Failed to create backup %v", req.BackupName)
		return nil, err
	}

	resp := &enginerpc.BackupCreateResponse{
		Backup:        backupStatus.Name,
		IsIncremental: backupStatus.IsIncremental,
	}

	logrus.Infof("Done initiating backup creation, received backupID: %v", resp.Backup)
	return resp, nil
}

func (s *SyncAgentServer) BackupStatus(ctx context.Context, req *enginerpc.BackupStatusRequest) (*enginerpc.BackupStatusResponse, error) {
	if req.Backup == "" {
		return nil, fmt.Errorf("bad request: empty backup name")
	}

	replicaObj, err := s.BackupList.BackupGet(req.Backup)
	if err != nil {
		return nil, err
	}

	snapshotName, err := diskutil.GetSnapshotNameFromDiskName(replicaObj.SnapshotID)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't get snapshot name")
	}

	resp := &enginerpc.BackupStatusResponse{
		Progress:     int32(replicaObj.Progress),
		BackupUrl:    replicaObj.BackupURL,
		Error:        replicaObj.Error,
		SnapshotName: snapshotName,
		State:        string(replicaObj.State),
	}
	return resp, nil
}

func (*SyncAgentServer) BackupRemove(ctx context.Context, req *enginerpc.BackupRemoveRequest) (*emptypb.Empty, error) {
	cmd := reexec.Command("backup", "delete", req.Backup)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	logrus.Infof("Running %s %v", cmd.Path, cmd.Args)
	if err := cmd.Wait(); err != nil {
		logrus.Infof("Error running %s %v: %v", "backup", cmd.Args, err)
		return nil, err
	}

	logrus.Infof("Done running %s %v", "backup", cmd.Args)
	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) waitForRestoreComplete() error {
	var (
		restoreProgress int
		restoreError    string
	)
	periodicChecker := time.NewTicker(PeriodicRefreshIntervalInSeconds * time.Second)

	for range periodicChecker.C {
		s.RestoreInfo.RLock()
		restoreProgress = s.RestoreInfo.Progress
		restoreError = s.RestoreInfo.Error
		s.RestoreInfo.RUnlock()

		if restoreProgress == 100 {
			logrus.Infof("Backup data restore completed successfully in Server")
			periodicChecker.Stop()
			return nil
		}
		if restoreError != "" {
			logrus.Errorf("Backup data restore Error Found in Server[%v]", restoreError)
			periodicChecker.Stop()
			return fmt.Errorf("%v", restoreError)
		}
	}
	return nil
}

func (s *SyncAgentServer) BackupRestore(ctx context.Context, req *enginerpc.BackupRestoreRequest) (e *emptypb.Empty, err error) {
	// Check request
	if req.SnapshotDiskName == "" {
		return nil, fmt.Errorf("empty snapshot disk name for the restore")
	}
	if req.Backup == "" {
		return nil, fmt.Errorf("empty backup URL for the restore")
	}

	backupType, err := butil.CheckBackupType(req.Backup)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to check the type for backup %v", req.Backup)
	}

	if err := butil.SetupCredential(backupType, req.Credential); err != nil {
		return nil, errors.Wrapf(err, "failed to setup credential for backup %v", req.Backup)
	}

	requestedBackupName, _, _, err := backupstore.DecodeBackupURL(util.UnescapeURL(req.Backup))
	if err != nil {
		return nil, err
	}

	if err := s.StartRestore(req.Backup, requestedBackupName, req.SnapshotDiskName, int(req.ConcurrentLimit)); err != nil {
		return nil, errors.Wrapf(err, "error starting backup restore")
	}

	go func() {
		if completeErr := s.completeBackupRestore(); completeErr != nil {
			logrus.WithError(completeErr).Warn("Failed to complete backup restore")
		}
	}()

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) completeBackupRestore() (err error) {
	defer func() {
		if extraErr := s.FinishRestore(err); extraErr != nil {
			logrus.WithError(extraErr).Error("Failed to finish backup restore")
			return
		}
	}()

	if err := s.waitForRestoreComplete(); err != nil {
		return errors.Wrapf(err, "failed to wait for restore complete")
	}

	s.RLock()
	restoreStatus := s.RestoreInfo.DeepCopy()
	s.RUnlock()

	if restoreStatus.LastRestored != "" {
		return s.postIncrementalRestoreOperations(restoreStatus)
	}
	return s.postFullRestoreOperations(restoreStatus)
}

func (s *SyncAgentServer) postFullRestoreOperations(restoreStatus *replica.RestoreStatus) error {
	if err := backup.CreateNewSnapshotMetafile(restoreStatus.ToFileName + ".meta"); err != nil {
		logrus.WithError(err).Error("Failed creating meta snapshot file")
		return err
	}

	// Check if this full restore is the fallback of the incremental restore
	if strings.HasSuffix(restoreStatus.ToFileName, ".snap_tmp") {
		if err := s.extraIncrementalFullRestoreOperations(restoreStatus); err != nil {
			logrus.WithError(err).Errorf("Failed to complete incremental fallback full restore")
			return err
		}
		logrus.Infof("Done running full restore %v to %v as the fallback of the incremental restore",
			restoreStatus.BackupURL, restoreStatus.ToFileName)
	} else {
		// Reload the replica so that the snapshot file can be loaded in the replica disk chain
		if err := s.reloadReplica(); err != nil {
			err = errors.Wrapf(err, "failed to reload replica %v for the full restore", s.replicaAddress)
			logrus.Error(err)
			return err
		}

		if err := s.replicaRevert(restoreStatus.ToFileName, time.Now().UTC().Format(time.RFC3339)); err != nil {
			err = errors.Wrapf(err, "failed to revert to %s for replica %s", restoreStatus.ToFileName, s.replicaAddress)
			logrus.Error(err)
			return err
		}
		logrus.Infof("Reverting to snapshot %s on %s successful", restoreStatus.ToFileName, s.replicaAddress)
	}

	logrus.Infof("Done running full restore %v to %v", restoreStatus.BackupURL, restoreStatus.ToFileName)
	return nil
}

func (s *SyncAgentServer) extraIncrementalFullRestoreOperations(restoreStatus *replica.RestoreStatus) error {
	tmpSnapshotDiskName := restoreStatus.ToFileName
	snapshotDiskName, err := diskutil.GetSnapshotNameFromTempFileName(tmpSnapshotDiskName)
	if err != nil {
		logrus.WithError(err).Error("Failed to get snapshotName from tempFileName")
		return err
	}
	snapshotDiskMetaName := diskutil.GenerateSnapshotDiskMetaName(snapshotDiskName)
	tmpSnapshotDiskMetaName := diskutil.GenerateSnapshotDiskMetaName(tmpSnapshotDiskName)

	defer func() {
		// try to clean up tmp files
		if _, err := s.FileRemove(context.TODO(), &enginerpc.FileRemoveRequest{
			FileName: tmpSnapshotDiskName,
		}); err != nil {
			logrus.WithError(err).Warnf("Failed to clean up delta file %s", tmpSnapshotDiskName)
		}

		if _, err := s.FileRemove(context.TODO(), &enginerpc.FileRemoveRequest{
			FileName: tmpSnapshotDiskMetaName,
		}); err != nil {
			logrus.WithError(err).Warnf("Failed to clean up delta file %s", tmpSnapshotDiskMetaName)
		}
	}()

	// Replace old snapshot and the related meta file
	if err := os.Rename(tmpSnapshotDiskName, snapshotDiskName); err != nil {
		return errors.Wrapf(err, "failed to replace old snapshot %v with the new fully restored file %v",
			snapshotDiskName, tmpSnapshotDiskName)
	}
	if err := os.Rename(tmpSnapshotDiskMetaName, snapshotDiskMetaName); err != nil {
		return errors.Wrapf(err, "failed to replace old snapshot meta file %v with the new restored meta file %v",
			snapshotDiskMetaName, tmpSnapshotDiskMetaName)
	}

	// Reload the replica as snapshot files got changed
	if err := s.reloadReplica(); err != nil {
		return errors.Wrapf(err, "failed to reload replica after the full restore")
	}

	return nil
}

func (s *SyncAgentServer) postIncrementalRestoreOperations(restoreStatus *replica.RestoreStatus) error {
	deltaFileName := restoreStatus.ToFileName
	logrus.Info("Cleaning up incremental restore by Coalescing and removing the delta file")
	defer func() {
		if _, err := s.FileRemove(context.TODO(), &enginerpc.FileRemoveRequest{
			FileName: deltaFileName,
		}); err != nil {
			logrus.WithError(err).Warnf("Failed to clean up delta file %s", deltaFileName)
		}
	}()

	// Coalesce delta file to snapshot/disk file
	if err := sparse.FoldFile(deltaFileName, restoreStatus.SnapshotDiskName, &PurgeStatus{}); err != nil {
		logrus.WithError(err).Errorf("Failed to coalesce %s on %s", deltaFileName, restoreStatus.SnapshotDiskName)
		return err
	}

	// Reload the replica as snapshot files got changed
	if err := s.reloadReplica(); err != nil {
		logrus.WithError(err).Error("Failed to reload replica")
		return err
	}

	return nil
}

func (s *SyncAgentServer) reloadReplica() error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()
	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	if _, err := replicaServiceClient.ReplicaReload(ctx, &emptypb.Empty{}); err != nil {
		return errors.Wrapf(err, "failed to reload replica %v", s.replicaAddress)
	}

	return nil
}

func (s *SyncAgentServer) replicaRevert(name, created string) error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()
	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	if _, err := replicaServiceClient.ReplicaRevert(ctx, &enginerpc.ReplicaRevertRequest{
		Name:    name,
		Created: created,
	}); err != nil {
		return errors.Wrapf(err, "failed to revert replica %v", s.replicaAddress)
	}

	return nil
}

func (s *SyncAgentServer) RestoreStatus(ctx context.Context, req *emptypb.Empty) (*enginerpc.RestoreStatusResponse, error) {
	resp := enginerpc.RestoreStatusResponse{
		IsRestoring: s.IsRestoring(),
	}

	if s.RestoreInfo == nil {
		return &resp, nil
	}

	restoreStatus := s.RestoreInfo.DeepCopy()
	resp.Progress = int32(restoreStatus.Progress)
	resp.DestFileName = restoreStatus.SnapshotDiskName
	resp.State = string(restoreStatus.State)
	resp.Error = restoreStatus.Error
	resp.BackupUrl = restoreStatus.BackupURL
	resp.LastRestored = restoreStatus.LastRestored
	resp.CurrentRestoringBackup = restoreStatus.CurrentRestoringBackup
	return &resp, nil
}

func (s *SyncAgentServer) SnapshotPurge(ctx context.Context, req *emptypb.Empty) (*emptypb.Empty, error) {
	if err := s.PreparePurge(); err != nil {
		return nil, err
	}

	go func() {
		if purgeErr := s.purgeSnapshots(); purgeErr != nil {
			logrus.WithError(purgeErr).Warn("Failed to purge snapshots")
		}
	}()

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) purgeSnapshots() (err error) {
	defer func() {
		s.PurgeStatus.Lock()
		if err != nil {
			s.PurgeStatus.Error = err.Error()
			s.PurgeStatus.State = types.ProcessStateError
		} else {
			s.PurgeStatus.State = types.ProcessStateComplete
		}
		s.PurgeStatus.Unlock()

		if err := s.FinishPurge(); err != nil {
			logrus.WithError(err).Error("Could not mark finish purge")
		}
	}()

	replicaClient, err := replicaclient.NewReplicaClient(s.replicaAddress, s.volumeName, s.instanceName)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := replicaClient.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()

	var leaves []string

	snapshotsInfo, _, err := getSnapshotsInfo(replicaClient)
	if err != nil {
		return err
	}

	for snapshot, info := range snapshotsInfo {
		if len(info.Children) == 0 {
			leaves = append(leaves, snapshot)
		}
		if info.Name == types.VolumeHeadName {
			continue
		}
		// Mark system generated snapshots as removed
		if !info.UserCreated && !info.Removed {
			if err := s.markSnapshotAsRemoved(snapshot); err != nil {
				return err
			}
		}
	}

	snapshotsInfo, markedRemoved, err := getSnapshotsInfo(replicaClient)
	if err != nil {
		return err
	}

	s.PurgeStatus.Lock()
	s.PurgeStatus.total = markedRemoved
	s.PurgeStatus.Unlock()

	var latestSnapshot string
	// We're tracing up from each leaf to the root
	var removed int
	for _, leaf := range leaves {
		// Somehow the leaf was removed during the process
		if _, ok := snapshotsInfo[leaf]; !ok {
			continue
		}
		snapshot := leaf
		for snapshot != "" {
			// Snapshot already removed? Skip to the next leaf
			info, ok := snapshotsInfo[snapshot]
			if !ok {
				break
			}
			if info.Removed {
				if info.Name == types.VolumeHeadName {
					return fmt.Errorf("BUG: Volume head was marked as removed")
				}
				// Process the snapshot directly behinds the volume head in the end
				if latestSnapshot == "" {
					for childName := range info.Children {
						if childName == types.VolumeHeadName {
							latestSnapshot = snapshot
							break
						}
					}
					if latestSnapshot != "" {
						snapshot = info.Parent
						continue
					}
				}
				if err := s.processRemoveSnapshot(snapshot); err != nil {
					return err
				}
				removed++
				s.PurgeStatus.Lock()
				s.PurgeStatus.processed = removed
				s.PurgeStatus.Unlock()
			}
			snapshot = info.Parent
		}
		// Update snapshotInfo in case some nodes have been removed
		snapshotsInfo, markedRemoved, err = getSnapshotsInfo(replicaClient)
		if err != nil {
			return err
		}
		s.PurgeStatus.Lock()
		s.PurgeStatus.total = markedRemoved + removed
		s.PurgeStatus.Progress = int(float32(removed) / float32(s.PurgeStatus.total) * 100)
		s.PurgeStatus.Unlock()
	}

	if latestSnapshot != "" {
		if err := s.processRemoveSnapshot(latestSnapshot); err != nil {
			return err
		}
		removed++
		s.PurgeStatus.Lock()
		s.PurgeStatus.processed = removed
		s.PurgeStatus.total++
		s.PurgeStatus.Progress = int(float32(removed) / float32(s.PurgeStatus.total) * 100)
		s.PurgeStatus.Unlock()
	}

	s.PurgeStatus.Lock()
	s.PurgeStatus.Progress = 100
	s.PurgeStatus.Unlock()

	return nil
}

func (s *SyncAgentServer) SnapshotPurgeStatus(ctx context.Context, req *emptypb.Empty) (*enginerpc.SnapshotPurgeStatusResponse, error) {
	isPurging := s.IsPurging()

	s.PurgeStatus.RLock()
	defer s.PurgeStatus.RUnlock()
	return &enginerpc.SnapshotPurgeStatusResponse{
		IsPurging: isPurging,
		Error:     s.PurgeStatus.Error,
		Progress:  int32(s.PurgeStatus.Progress),
		State:     string(s.PurgeStatus.State),
	}, nil
}

func (s *SyncAgentServer) PreparePurge() error {
	s.Lock()
	defer s.Unlock()

	if s.isPurging {
		return fmt.Errorf("replica is already purging snapshots")
	}

	s.isPurging = true

	s.PurgeStatus.Lock()
	s.PurgeStatus.Error = ""
	s.PurgeStatus.Progress = 0
	s.PurgeStatus.State = types.ProcessStateInProgress
	s.PurgeStatus.total = 0
	s.PurgeStatus.processed = 0
	s.PurgeStatus.Unlock()

	return nil
}

func (s *SyncAgentServer) FinishPurge() error {
	s.Lock()
	defer s.Unlock()

	if !s.isPurging {
		return fmt.Errorf("BUG: replica is not purging snapshots")
	}

	s.isPurging = false
	return nil
}

func (s *SyncAgentServer) IsPurging() bool {
	s.RLock()
	defer s.RUnlock()

	return s.isPurging
}

func getSnapshotsInfo(replicaClient *replicaclient.ReplicaClient) (map[string]types.DiskInfo, int, error) {
	resp, err := replicaClient.GetReplica()
	if err != nil {
		return nil, 0, err
	}

	disks := make(map[string]types.DiskInfo)
	for name, disk := range resp.Disks {
		if name == resp.BackingFile {
			continue
		}
		disks[name] = disk
	}

	newDisks := make(map[string]types.DiskInfo)
	removedCount := 0
	for name, disk := range disks {
		snapshot := ""

		if !diskutil.IsHeadDisk(name) {
			snapshot, err = diskutil.GetSnapshotNameFromDiskName(name)
			if err != nil {
				return nil, 0, err
			}
		} else {
			snapshot = types.VolumeHeadName
		}
		children := map[string]bool{}
		for childDisk := range disk.Children {
			child := ""
			if !diskutil.IsHeadDisk(childDisk) {
				child, err = diskutil.GetSnapshotNameFromDiskName(childDisk)
				if err != nil {
					return nil, 0, err
				}
			} else {
				child = types.VolumeHeadName
			}
			children[child] = true
		}
		parent := ""
		if disk.Parent != "" {
			parent, err = diskutil.GetSnapshotNameFromDiskName(disk.Parent)
			if err != nil {
				return nil, 0, err
			}
		}

		if disk.Removed {
			removedCount++
		}

		info := types.DiskInfo{
			Name:        snapshot,
			Parent:      parent,
			Removed:     disk.Removed,
			UserCreated: disk.UserCreated,
			Children:    children,
			Created:     disk.Created,
			Size:        disk.Size,
			Labels:      disk.Labels,
		}
		newDisks[snapshot] = info
	}

	return newDisks, removedCount, nil
}

func (s *SyncAgentServer) markSnapshotAsRemoved(snapshot string) error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()

	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	if _, err := replicaServiceClient.DiskMarkAsRemoved(ctx, &enginerpc.DiskMarkAsRemovedRequest{
		Name: snapshot,
	}); err != nil {
		return err
	}

	return nil
}

func (s *SyncAgentServer) processRemoveSnapshot(snapshot string) error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()

	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	ops, err := replicaServiceClient.DiskPrepareRemove(ctx, &enginerpc.DiskPrepareRemoveRequest{
		Name: snapshot,
	})
	if err != nil {
		return err
	}

	for _, op := range ops.Operations {
		switch op.Action {
		case replica.OpCoalesce:
			logrus.Infof("Coalescing %v to %v", op.Target, op.Source)
			if err := sparse.FoldFile(op.Target, op.Source, s.PurgeStatus); err != nil {
				logrus.WithError(err).Errorf("Failed to coalesce %v to %v", op.Target, op.Source)
				return err
			}
		case replica.OpRemove:
			logrus.Infof("Removing %v", op.Source)
			if err := s.rmDisk(op.Source); err != nil {
				logrus.WithError(err).Errorf("Failed to remove %v", op.Source)
				return err
			}
		case replica.OpReplace:
			logrus.Infof("Replacing %v with %v", op.Source, op.Target)
			if err = s.replaceDisk(op.Source, op.Target); err != nil {
				logrus.WithError(err).Errorf("Failed to replace %v with %v", op.Source, op.Target)
				return err
			}
		case replica.OpPrune:
			logrus.Infof("Pruning overlapping chunks from %v based on %v", op.Source, op.Target)
			if err := sparse.PruneFile(op.Source, op.Target, s.PurgeStatus); err != nil {
				logrus.WithError(err).Errorf("Failed to prune %v based on %v", op.Source, op.Target)
				return err
			}
		}
	}

	return nil
}

func (s *SyncAgentServer) replaceDisk(source, target string) error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()

	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	if _, err := replicaServiceClient.DiskReplace(ctx, &enginerpc.DiskReplaceRequest{
		Source: source,
		Target: target,
	}); err != nil {
		return err
	}

	return nil
}

func (s *SyncAgentServer) rmDisk(disk string) error {
	conn, err := grpc.NewClient(s.replicaAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		interceptor.WithIdentityValidationClientInterceptor(s.volumeName, s.instanceName))
	if err != nil {
		return errors.Wrapf(err, "cannot connect to ReplicaService %v", s.replicaAddress)
	}
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			logrus.WithError(errClose).Errorf("Failed to close replica client for for replica address %v", s.replicaAddress)
		}
	}()

	replicaServiceClient := enginerpc.NewReplicaServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), GRPCServiceCommonTimeout)
	defer cancel()

	if _, err := replicaServiceClient.DiskRemove(ctx, &enginerpc.DiskRemoveRequest{
		Force: false,
		Name:  disk,
	}); err != nil {
		return err
	}

	return nil
}

func (s *SyncAgentServer) SnapshotHash(ctx context.Context, req *enginerpc.SnapshotHashRequest) (*emptypb.Empty, error) {
	if req.SnapshotName == "" {
		return nil, fmt.Errorf("snapshot name is required")
	}

	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		task := replica.NewSnapshotHashJob(ctx, cancel, req.SnapshotName, req.Rehash)

		if err := s.SnapshotHashList.Add(req.SnapshotName, task); err != nil {
			logrus.WithError(err).Errorf("failed to add snapshot %v for hashing", req.SnapshotName)
			return
		}

		if err := task.Execute(); err != nil {
			logrus.WithError(err).Errorf("failed to hash snapshot %v", req.SnapshotName)
		}
	}()

	return &emptypb.Empty{}, nil
}

func checkSnapshotHashStatusFromChecksumFile(snapshotName string) (string, error) {
	info, err := replica.GetSnapshotHashInfoFromChecksumFile(snapshotName)
	if err != nil {
		return "", err
	}

	checksum := info.Checksum
	changeTime := info.ChangeTime

	currentChangeTime, err := replica.GetSnapshotChangeTime(snapshotName)
	if err != nil {
		return "", err
	}

	if changeTime != currentChangeTime {
		return "", fmt.Errorf("snapshot %v is changed", snapshotName)
	}

	return checksum, nil
}

func (s *SyncAgentServer) SnapshotHashStatus(ctx context.Context, req *enginerpc.SnapshotHashStatusRequest) (*enginerpc.SnapshotHashStatusResponse, error) {
	// By default, the hash status should be retrieved from SnapshotHashList.
	// After finishing the hash task, the state becomes complete and checksum file is set. If the hash status in the SnapshotHashList is
	// somehow cleaned up by refresh(), the result can be read from checksum file instead.
	// If the state cannot be found in the checksum file, it implicitly indicate the hashing task failed.
	task, err := s.SnapshotHashList.Get(req.SnapshotName)
	if err != nil {
		checksum, err := checkSnapshotHashStatusFromChecksumFile(req.SnapshotName)
		if err != nil {
			return &enginerpc.SnapshotHashStatusResponse{
				State: string(replica.ProgressStateError),
				Error: err.Error(),
			}, nil
		}

		return &enginerpc.SnapshotHashStatusResponse{
			State:    string(replica.ProgressStateComplete),
			Checksum: checksum,
		}, nil
	}

	task.StatusLock.RLock()
	defer task.StatusLock.RUnlock()
	return &enginerpc.SnapshotHashStatusResponse{
		State:             string(task.State),
		Checksum:          task.Checksum,
		Error:             task.Error,
		SilentlyCorrupted: task.SilentlyCorrupted,
	}, nil
}

func (s *SyncAgentServer) SnapshotHashCancel(ctx context.Context, req *enginerpc.SnapshotHashCancelRequest) (*emptypb.Empty, error) {
	if req.SnapshotName == "" {
		return nil, fmt.Errorf("snapshot name is required")
	}

	task, err := s.SnapshotHashList.Get(req.SnapshotName)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return &emptypb.Empty{}, nil
		}
		return nil, errors.Wrapf(err, "failed to cancel snapshot %v hash task", req.SnapshotName)
	}

	task.CancelFunc()

	return &emptypb.Empty{}, nil
}

func (s *SyncAgentServer) SnapshotHashLockState(ctx context.Context, req *emptypb.Empty) (*enginerpc.SnapshotHashLockStateResponse, error) {
	err := os.MkdirAll(replica.FileLockDirectory, 0755)
	if err != nil {
		return nil, err
	}

	fileLock := flock.New(filepath.Join(replica.FileLockDirectory, replica.HashLockFileName))

	isLocked, err := fileLock.TryLock()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to try lock %v", fileLock)
	}
	defer func() {
		if unlockErr := fileLock.Unlock(); unlockErr != nil {
			logrus.WithError(unlockErr).WithFields(logrus.Fields{
				"file": fileLock.Path(),
			}).Warn("Failed to unlock file")
		}
	}()

	return &enginerpc.SnapshotHashLockStateResponse{
		IsLocked: !isLocked,
	}, nil
}
