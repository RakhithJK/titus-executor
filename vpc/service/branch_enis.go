package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"time"

	"k8s.io/client-go/util/workqueue"

	"github.com/Netflix/titus-executor/logger"
	"github.com/Netflix/titus-executor/vpc/service/ec2wrapper"
	"github.com/Netflix/titus-executor/vpc/tracehelpers"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.opencensus.io/trace"

	"github.com/Netflix/titus-executor/aws/aws-sdk-go/aws"
	"github.com/Netflix/titus-executor/aws/aws-sdk-go/service/ec2"
	"github.com/lib/pq"
)

const (
	maxAssociateTime = 10 * time.Second
)

func insertBranchENIIntoDB(ctx context.Context, tx *sql.Tx, iface *ec2.NetworkInterface) error {
	securityGroupIds := make([]string, len(iface.Groups))
	for idx := range iface.Groups {
		securityGroupIds[idx] = aws.StringValue(iface.Groups[idx].GroupId)
	}
	sort.Strings(securityGroupIds)

	_, err := tx.ExecContext(ctx, "INSERT INTO branch_enis (branch_eni, subnet_id, account_id, az, vpc_id, security_groups, modified_at) VALUES ($1, $2, $3, $4, $5, $6, transaction_timestamp()) ON CONFLICT (branch_eni) DO NOTHING",
		aws.StringValue(iface.NetworkInterfaceId),
		aws.StringValue(iface.SubnetId),
		aws.StringValue(iface.OwnerId),
		aws.StringValue(iface.AvailabilityZone),
		aws.StringValue(iface.VpcId),
		pq.Array(securityGroupIds),
	)

	return err
}

// We are given the session of the trunk ENI account
// We assume network interface permissions are already taken care of
type association struct {
	branchENI string
	trunkENI  string
}

func (vpcService *vpcService) associateNetworkInterface(ctx context.Context, tx *sql.Tx, session *ec2wrapper.EC2Session, association association, idx int) (*string, error) {
	ctx, span := trace.StartSpan(ctx, "associateNetworkInterface")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, maxAssociateTime)
	defer cancel()

	branchENI := association.branchENI
	trunkENI := association.trunkENI
	span.AddAttributes(
		trace.StringAttribute("branchENI", branchENI),
		trace.StringAttribute("trunkENI", trunkENI),
		trace.Int64Attribute("idx", int64(idx)))

	id, err := vpcService.startAssociation(ctx, branchENI, trunkENI, idx)
	if err != nil {
		err = errors.Wrap(err, "Unable to start association")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	associationID, err := vpcService.finishAssociation(ctx, tx, session, id)
	if err != nil {
		logger.G(ctx).WithError(err).Error("Unable to finish association")
		err = errors.Wrap(err, "Unable to finish association")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	return associationID, nil
}

type persistentError struct {
	err error
}

func (p *persistentError) Unwrap() error {
	return p.err
}

func (p *persistentError) Error() string {
	return p.err.Error()
}

func (p *persistentError) Is(target error) bool {
	_, ok := target.(*persistentError)
	return ok
}

func (vpcService *vpcService) finishAssociation(ctx context.Context, tx *sql.Tx, session *ec2wrapper.EC2Session, id int) (*string, error) {
	ctx, span := trace.StartSpan(ctx, "finishAssociation")
	defer span.End()

	span.AddAttributes(trace.Int64Attribute("id", int64(id)))

	row := tx.QueryRowContext(ctx, `
SELECT branch_eni_actions_associate.token,
       branch_eni_actions_associate.association_id,
       branch_eni_actions_associate.branch_eni,
       branch_eni_actions_associate.trunk_eni,
       branch_eni_actions_associate.idx,
       trunk_enis.account_id,
       trunk_enis.region,
       branch_eni_actions_associate.state
FROM branch_eni_actions_associate
JOIN trunk_enis ON branch_eni_actions_associate.trunk_eni = trunk_enis.trunk_eni
WHERE branch_eni_actions_associate.id = $1
FOR NO KEY UPDATE OF branch_eni_actions_associate
`, id)

	var associationID sql.NullString
	var token, branchENI, trunkENI, accountID, region, state string
	var idx int

	err := row.Scan(&token, &associationID, &branchENI, &trunkENI, &idx, &accountID, &region, &state)
	if err != nil {
		err = errors.Wrap(err, "Cannot scan association action")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	if state == "completed" {
		if associationID.Valid {
			return &associationID.String, nil
		}
		err = errors.New("State is completed, but associationID is null")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	if state == "failed" {
		row = tx.QueryRowContext(ctx, `
SELECT branch_eni_actions_associate.error_code,
       branch_eni_actions_associate.error_message
FROM branch_eni_actions_associate
WHERE branch_eni_actions_associate.id = $1
`, id)
		var errorCode, errorMessage string
		err = row.Scan(&errorCode, &errorMessage)
		if err != nil {
			err = errors.Wrap(err, "Cannot scan association action for error")
			tracehelpers.SetStatus(err, span)
			return nil, err
		}

		err = fmt.Errorf("Request failed with code: %q, message: %q", errorCode, errorMessage)
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	// TODO: When this version of the code is fully rolled out, we can remove this line of code, and just insert the whole branch_eni_attachments
	// at once
	row = tx.QueryRowContext(ctx, "INSERT INTO branch_eni_attachments(branch_eni, trunk_eni, idx, attachment_generation) VALUES ($1, $2, $3, 3) RETURNING id",
		branchENI, trunkENI, idx)
	var branchENIAttachmentID int
	err = row.Scan(&branchENIAttachmentID)
	if err != nil {
		err = errors.Wrap(err, "Cannot create row in branch ENI attachments")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	if session == nil {
		session, err = vpcService.ec2.GetSessionFromAccountAndRegion(ctx, ec2wrapper.Key{AccountID: accountID, Region: region})
		if err != nil {
			err = errors.Wrap(err, "Could not get session")
			tracehelpers.SetStatus(err, span)
			return nil, err
		}
	}

	output, err := session.AssociateTrunkInterface(ctx, ec2.AssociateTrunkInterfaceInput{
		TrunkInterfaceId:  aws.String(trunkENI),
		BranchInterfaceId: aws.String(branchENI),
		ClientToken:       aws.String(token),
		VlanId:            aws.Int64(int64(idx)),
	})
	if err != nil {
		logger.G(ctx).WithError(err).Error("Unable to associate trunk network interface")
		awsErr := ec2wrapper.RetrieveEC2Error(err)
		// This likely means that the association never succeeded.
		if awsErr != nil {
			logger.G(ctx).WithError(awsErr).Error("Unable to associate trunk network interface due to underlying AWS issue")
			_, err2 := tx.ExecContext(ctx,
				"UPDATE branch_eni_actions_associate SET state = 'failed', completed_by = $1, completed_at = now(), error_code = $2, error_message = $3  WHERE id = $4",
				vpcService.hostname, awsErr.Code(), awsErr.Message(), id)
			if err2 != nil {
				logger.G(ctx).WithError(err).Error("Unable to update branch_eni_actions table to mark action as failed")
				tracehelpers.SetStatus(err, span)
				return nil, ec2wrapper.HandleEC2Error(err, span)
			}

			// We need to delete the row from branch_eni_attachments we inserted earlier
			_, err2 = tx.ExecContext(ctx, "DELETE FROM branch_eni_attachments WHERE id = $1", branchENIAttachmentID)
			if err2 != nil {
				logger.G(ctx).WithError(err).Error("Unable to delete dangling entry in branch eni attachments table")
				return nil, ec2wrapper.HandleEC2Error(err, span)
			}
			return nil, &persistentError{err: ec2wrapper.HandleEC2Error(err, span)}

		}
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	_, err = tx.ExecContext(ctx,
		"UPDATE branch_eni_actions_associate SET state = 'completed', completed_by = $1, completed_at = now(), association_id = $2 WHERE id = $3",
		vpcService.hostname, aws.StringValue(output.InterfaceAssociation.AssociationId), id)
	if err != nil {
		err = errors.Wrap(err, "Cannot update branch_eni_actions table to mark action as completed")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	_, err = tx.ExecContext(ctx, "UPDATE branch_eni_attachments SET association_id = $1 WHERE id = $2",
		aws.StringValue(output.InterfaceAssociation.AssociationId), branchENIAttachmentID)
	if err != nil {
		err = errors.Wrap(err, "Cannot update branch_eni_attachments table with populated attachment")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	_, err = tx.ExecContext(ctx, "SELECT pg_notify('branch_eni_actions_associate_finished', $1)", strconv.Itoa(id))
	if err != nil {
		// These errors might largely be recoverable, so you know, deal with that
		err = errors.Wrap(err, "Unable to notify of branch_eni_actions_associate_finished")
		tracehelpers.SetStatus(err, span)
		return nil, err
	}

	return output.InterfaceAssociation.AssociationId, nil
}

func (vpcService *vpcService) startAssociation(ctx context.Context, branchENI, trunkENI string, idx int) (int, error) {
	ctx, span := trace.StartSpan(ctx, "startAssociation")
	defer span.End()

	// Get that predicate locking action.
	tx, err := vpcService.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelSerializable,
	})
	if err != nil {
		err = errors.Wrap(err, "Could not start database transaction")
		tracehelpers.SetStatus(err, span)
		return 0, err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	row := tx.QueryRowContext(ctx,
		"SELECT branch_eni, association_id FROM branch_eni_attachments WHERE trunk_eni = $1 AND idx = $2 LIMIT 1",
		trunkENI, idx)
	var existingBranchENI, existingAssociationID string
	err = row.Scan(&existingBranchENI, &existingAssociationID)

	if err != sql.ErrNoRows {
		_ = tx.Rollback()
		if err != nil {
			err = errors.Wrap(err, "error querying branch_eni_attachments to hold predicate lock")
			tracehelpers.SetStatus(err, span)
			return 0, err
		}
		err = fmt.Errorf("Conflicting association ID %q already from branch ENI %q on trunk ENI %q at index %d", existingAssociationID, existingBranchENI, trunkENI, idx)
		tracehelpers.SetStatus(err, span)
		return 0, err
	}

	row = tx.QueryRowContext(ctx,
		"SELECT trunk_eni, association_id FROM branch_eni_attachments WHERE branch_eni = $1 LIMIT 1", branchENI)
	var existingTrunkENIID string
	err = row.Scan(&existingTrunkENIID, &existingAssociationID)
	if err != sql.ErrNoRows {
		_ = tx.Rollback()
		if err != nil {
			err = errors.Wrap(err, "error querying branch_eni_attachments to hold predicate lock")
			tracehelpers.SetStatus(err, span)
			return 0, err
		}
		err = fmt.Errorf("Conflicting association: Branch ENI %q already associated with trunk ENI %q (association: %s)",
			branchENI, existingTrunkENIID, existingAssociationID)
		tracehelpers.SetStatus(err, span)
		return 0, err
	}

	// TODO: Consider making a foreign key relationship between branch_eni_actions and branch_eni_attachments
	clientToken := uuid.New().String()
	row = tx.QueryRowContext(ctx,
		"INSERT INTO branch_eni_actions_associate(token, branch_eni, trunk_eni, idx, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		clientToken, branchENI, trunkENI, idx, vpcService.hostname,
	)

	var id int
	err = row.Scan(&id)
	if err != nil {
		// These errors might largely be recoverable, so you know, deal with that
		err = errors.Wrap(err, "Unable to insert and scan into branch_eni_actions")
		tracehelpers.SetStatus(err, span)
		return 0, err
	}
	span.AddAttributes(trace.Int64Attribute("id", int64(id)))

	_, err = tx.ExecContext(ctx, "SELECT pg_notify('branch_eni_actions_associate_created', $1)", strconv.Itoa(id))
	if err != nil {
		// These errors might largely be recoverable, so you know, deal with that
		err = errors.Wrap(err, "Unable to notify of branch_eni_actions_associate_created")
		tracehelpers.SetStatus(err, span)
		return 0, err
	}

	err = tx.Commit()
	if err != nil {
		err = errors.Wrap(err, "Unable to commit transaction")
		tracehelpers.SetStatus(err, span)
		return 0, err
	}

	logger.G(ctx).WithFields(map[string]interface{}{
		"branchENI": branchENI,
		"trunkENI":  trunkENI,
	})

	return id, nil
}

func (vpcService *vpcService) ensureBranchENIPermissionV3(ctx context.Context, tx *sql.Tx, trunkENIAccountID string, branchENISession *ec2wrapper.EC2Session, eni *branchENI) error {
	ctx, span := trace.StartSpan(ctx, "ensureBranchENIPermissionV3")
	defer span.End()

	if eni.accountID == trunkENIAccountID {
		return nil
	}

	// This could be collapsed into a join on the above query, but for now, we wont do that
	row := tx.QueryRowContext(ctx, "SELECT COALESCE(count(*), 0) FROM eni_permissions WHERE branch_eni = $1 AND account_id = $2", eni.id, trunkENIAccountID)
	var permissions int
	err := row.Scan(&permissions)
	if err != nil {
		err = errors.Wrap(err, "Cannot retrieve from branch ENI permissions")
		span.SetStatus(traceStatusFromError(err))
		return err
	}
	if permissions > 0 {
		return nil
	}

	logger.G(ctx).Debugf("Creating network interface permission to allow account %s to attach branch ENI in account %s", trunkENIAccountID, eni.accountID)
	ec2client := ec2.New(branchENISession.Session)
	_, err = ec2client.CreateNetworkInterfacePermissionWithContext(ctx, &ec2.CreateNetworkInterfacePermissionInput{
		AwsAccountId:       aws.String(trunkENIAccountID),
		NetworkInterfaceId: aws.String(eni.id),
		Permission:         aws.String("INSTANCE-ATTACH"),
	})

	if err != nil {
		err = errors.Wrap(err, "Cannot create network interface permission")
		span.SetStatus(traceStatusFromError(err))
		return err
	}

	_, err = tx.ExecContext(ctx, "INSERT INTO eni_permissions(branch_eni, account_id) VALUES ($1, $2) ON CONFLICT DO NOTHING ", eni.id, trunkENIAccountID)
	if err != nil {
		err = errors.Wrap(err, "Cannot insert network interface permission into database")
		span.SetStatus(traceStatusFromError(err))
		return err
	}

	return nil
}

type listenerEvent struct {
	listenerEvent pq.ListenerEventType
	err           error
}

func (vpcService *vpcService) branchENIAssociatorListener(ctx context.Context, item keyedItem) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	listenerEventCh := make(chan listenerEvent, 10)
	eventCallback := func(event pq.ListenerEventType, err error) {
		listenerEventCh <- listenerEvent{listenerEvent: event, err: err}
	}
	pqListener := pq.NewListener(vpcService.dbURL, 10*time.Second, 2*time.Minute, eventCallback)
	defer func() {
		_ = pqListener.Close()
	}()

	err := pqListener.Listen("branch_eni_actions_associate_created")
	if err != nil {
		return errors.Wrap(err, "Cannot listen on branch_eni_actions_associate_created channel")
	}
	err = pqListener.Listen("branch_eni_actions_associate_finished")
	if err != nil {
		return errors.Wrap(err, "Cannot listen on branch_eni_actions_associate_finished channel")
	}

	pingTimer := time.NewTimer(10 * time.Second)
	pingCh := make(chan error)

	wq := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "branchENIAssociator")
	defer wq.ShutDown()

	errCh := make(chan error)
	go func() {
		errCh <- vpcService.branchENIAssociatorWorker(ctx, wq)
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err = <-errCh:
			logger.G(ctx).WithError(err).Error("Worker exiting")
			return err
		case <-pingTimer.C:
			go func() {
				pingCh <- pqListener.Ping()
			}()
		case pingErr := <-pingCh:
			if pingErr != nil {
				logger.G(ctx).WithError(pingErr).Error("Could not ping database")
			}
			pingTimer.Reset(10 * time.Second)
		case ev := <-pqListener.Notify:
			// This is a reconnect event
			if ev == nil {
				err = vpcService.retrieveAllWorkItems(ctx, "branch_eni_actions_associate", wq)
				if err != nil {
					err = errors.Wrap(err, "Could not retrieve all work items after reconnecting to postgres")
					return err
				}
			} else if ev.Channel == "branch_eni_actions_associate_created" {
				logger.G(ctx).WithField("extra", ev.Extra).Debug("Received work item")
				wq.Add(ev.Extra)
			} else if ev.Channel == "branch_eni_actions_associate_finished" {
				logger.G(ctx).WithField("extra", ev.Extra).Debug("Received work finished")
				wq.Forget(ev.Extra)
			}
		case ev := <-listenerEventCh:
			switch ev.listenerEvent {
			case pq.ListenerEventConnected:
				logger.G(ctx).Info("Connected to postgres")
				err = vpcService.retrieveAllWorkItems(ctx, "branch_eni_actions_associate", wq)
				if err != nil {
					err = errors.Wrap(err, "Could not retrieve all work items")
					return err
				}
			case pq.ListenerEventDisconnected:
				wq.ShutDown()
				logger.G(ctx).WithError(ev.err).Error("Disconnected from postgres, stopping work")
			case pq.ListenerEventReconnected:
				logger.G(ctx).Info("Reconnected to postgres")
			case pq.ListenerEventConnectionAttemptFailed:
				// Maybe this should be case for the worker bailing?
				logger.G(ctx).WithError(ev.err).Error("Failed to reconnect to postgres")
			}
		}
	}
}

func (vpcService *vpcService) branchENIAssociatorWorker(ctx context.Context, wq workqueue.RateLimitingInterface) error {
	doWorkItem := func(item interface{}) error {
		ctx, span := trace.StartSpan(ctx, "doWorkItem")
		defer span.End()
		defer wq.Done(item)
		stringKey := item.(string)
		id, err := strconv.Atoi(stringKey)
		if err != nil {
			return errors.Wrapf(err, "Unable to parse key %q into id", stringKey)
		}
		ctx = logger.WithField(ctx, "id", id)

		logger.G(ctx).Debug("Processing work item")
		defer logger.G(ctx).Debug("Finished processing work item")

		tx, err := vpcService.db.BeginTx(ctx, &sql.TxOptions{})
		if err != nil {
			err = errors.Wrap(err, "Could not start database transaction")
			tracehelpers.SetStatus(err, span)
			return err
		}
		defer func() {
			_ = tx.Rollback()
		}()
		_, err = vpcService.finishAssociation(ctx, tx, nil, id)
		if errors.Is(err, &persistentError{}) {
			logger.G(ctx).WithError(err).Error("Experienced persistent error, still committing database state")
		} else if err != nil {
			tracehelpers.SetStatus(err, span)
			logger.G(ctx).WithError(err).Error("Failed to process item")
			wq.AddRateLimited(item)
			return nil
		}
		err = tx.Commit()
		if err != nil {
			err = errors.Wrap(err, "Could not commit database transaction")
			tracehelpers.SetStatus(err, span)
			wq.AddRateLimited(item)
			return nil
		}

		wq.Forget(item)
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		item, shuttingDown := wq.Get()
		if shuttingDown {
			return nil
		}
		err := doWorkItem(item)
		if err != nil {
			logger.G(ctx).WithError(err).Error("Received error from work function, exiting")
			return err
		}
	}
}

func (vpcService *vpcService) retrieveAllWorkItems(ctx context.Context, table string, wq workqueue.RateLimitingInterface) error {
	ctx, span := trace.StartSpan(ctx, "retrieveAllWorkItems")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	span.AddAttributes(trace.StringAttribute("table", table))

	tx, err := vpcService.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		err = errors.Wrap(err, "Could not start database transaction")
		tracehelpers.SetStatus(err, span)
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT id FROM %s WHERE state = 'pending'", table)) //nolint:gosec
	if err != nil {
		err = errors.Wrap(err, "Could not query table for pending work items")
		tracehelpers.SetStatus(err, span)
		return err
	}

	for rows.Next() {
		var id int
		err = rows.Scan(&id)
		if err != nil {
			err = errors.Wrap(err, "Could not scan work item")
			tracehelpers.SetStatus(err, span)
			return err
		}
		wq.Add(strconv.Itoa(id))
	}

	err = tx.Commit()
	if err != nil {
		err = errors.Wrap(err, "Could not commit transaction")
		tracehelpers.SetStatus(err, span)
		return err
	}

	return nil
}
