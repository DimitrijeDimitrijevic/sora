package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/migadu/sora/consts"
	"github.com/migadu/sora/helpers"
)

// CopyMessages copies multiple messages from a source mailbox to a destination mailbox within a given transaction.
// It returns a map of old UIDs to new UIDs.
func (db *Database) CopyMessages(ctx context.Context, tx pgx.Tx, uids *[]imap.UID, srcMailboxID, destMailboxID int64, userID int64) (map[imap.UID]imap.UID, error) {
	messageUIDMap := make(map[imap.UID]imap.UID)
	if srcMailboxID == destMailboxID {
		return nil, fmt.Errorf("source and destination mailboxes cannot be the same")
	}

	// The caller is responsible for beginning and committing/rolling back the transaction.

	// Get the source message IDs and UIDs
	rows, err := tx.Query(ctx, `SELECT id, uid FROM messages WHERE mailbox_id = $1 AND uid = ANY($2) AND expunged_at IS NULL ORDER BY uid`, srcMailboxID, uids)
	if err != nil {
		return nil, consts.ErrInternalError
	}
	defer rows.Close()

	var messageIDs []int64
	var sourceUIDsForMap []imap.UID
	for rows.Next() {
		var messageID int64
		var sourceUID imap.UID
		if err := rows.Scan(&messageID, &sourceUID); err != nil {
			return nil, fmt.Errorf("failed to scan message ID and UID: %w", err)
		}
		messageIDs = append(messageIDs, messageID)
		sourceUIDsForMap = append(sourceUIDsForMap, sourceUID)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating through source messages: %w", err)
	}

	if len(messageIDs) == 0 {
		return messageUIDMap, nil // No messages to copy
	}

	// Atomically increment highest_uid for the number of messages being copied.
	var newHighestUID int64
	numToCopy := int64(len(messageIDs))
	err = tx.QueryRow(ctx, `UPDATE mailboxes SET highest_uid = highest_uid + $1 WHERE id = $2 RETURNING highest_uid`, numToCopy, destMailboxID).Scan(&newHighestUID)
	if err != nil {
		return nil, consts.ErrDBUpdateFailed
	}

	// Calculate the new UIDs for the copied messages.
	var newUIDs []int64
	startUID := newHighestUID - numToCopy + 1
	for i, sourceUID := range sourceUIDsForMap {
		newUID := startUID + int64(i)
		newUIDs = append(newUIDs, newUID)
		messageUIDMap[sourceUID] = imap.UID(newUID)
	}

	// Fetch destination mailbox name within the same transaction
	var destMailboxName string
	if err := tx.QueryRow(ctx, "SELECT name FROM mailboxes WHERE id = $1", destMailboxID).Scan(&destMailboxName); err != nil {
		return nil, fmt.Errorf("failed to get destination mailbox name: %w", err)
	}

	// Batch insert the copied messages
	_, err = tx.Exec(ctx, `
		INSERT INTO messages (
			account_id, content_hash, uploaded, message_id, in_reply_to, 
			subject, sent_date, internal_date, flags, custom_flags, size, 
			body_structure, recipients_json, s3_domain, s3_localpart,
			subject_sort, from_name_sort, from_email_sort, to_email_sort, cc_email_sort,
			mailbox_id, mailbox_path, flags_changed_at, created_modseq, uid
		)
		SELECT 
			m.account_id, m.content_hash, m.uploaded, m.message_id, m.in_reply_to,
			m.subject, m.sent_date, m.internal_date, m.flags | $5, m.custom_flags, m.size,
			m.body_structure, m.recipients_json, m.s3_domain, m.s3_localpart,
			m.subject_sort, m.from_name_sort, m.from_email_sort, m.to_email_sort, m.cc_email_sort,
			$1 AS mailbox_id,
			$2 AS mailbox_path, -- Use the fetched destination mailbox name
			NOW() AS flags_changed_at,
			nextval('messages_modseq'),
			d.new_uid
		FROM messages m
		JOIN unnest($3::bigint[], $4::bigint[]) AS d(message_id, new_uid) ON m.id = d.message_id
	`, destMailboxID, destMailboxName, messageIDs, newUIDs, FlagRecent)
	if err != nil {
		return nil, fmt.Errorf("failed to batch copy messages: %w", err)
	}

	return messageUIDMap, nil
}

type InsertMessageOptions struct {
	UserID      int64
	MailboxID   int64
	MailboxName string
	S3Domain    string
	S3Localpart string
	ContentHash string
	MessageID   string
	// CustomFlags are handled by splitting options.Flags in InsertMessage
	Flags                []imap.Flag
	InternalDate         time.Time
	Size                 int64
	Subject              string
	PlaintextBody        string
	SentDate             time.Time
	InReplyTo            []string
	BodyStructure        *imap.BodyStructure
	Recipients           []helpers.Recipient
	RawHeaders           string
	PreservedUID         *uint32       // Optional: preserved UID from import
	PreservedUIDValidity *uint32       // Optional: preserved UIDVALIDITY from import
	FTSRetention         time.Duration // Optional: FTS retention period to skip old messages
}

func (d *Database) InsertMessage(ctx context.Context, tx pgx.Tx, options *InsertMessageOptions, upload PendingUpload) (messageID int64, uid int64, err error) {
	saneMessageID := helpers.SanitizeUTF8(options.MessageID)
	if saneMessageID == "" {
		log.Printf("[DB] messageID is empty after sanitization, generating a new one without modifying the message.")
		// Generate a new message ID if not provided
		saneMessageID = fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), options.MailboxName)
	}

	bodyStructureData, err := helpers.SerializeBodyStructureGob(options.BodyStructure)
	if err != nil {
		log.Printf("[DB] failed to serialize BodyStructure: %v", err)
		return 0, 0, consts.ErrSerializationFailed
	}

	if options.InternalDate.IsZero() {
		options.InternalDate = time.Now()
	}

	var highestUID int64
	var uidToUse int64

	// If we have a preserved UID, use it; otherwise generate a new one
	if options.PreservedUID != nil {
		uidToUse = int64(*options.PreservedUID)

		// Update highest_uid if preserved UID is higher
		err = tx.QueryRow(ctx, `
			UPDATE mailboxes 
			SET highest_uid = GREATEST(highest_uid, $2)
			WHERE id = $1 
			RETURNING highest_uid`,
			options.MailboxID, uidToUse).Scan(&highestUID)
		if err != nil {
			log.Printf("[DB] failed to update highest UID with preserved UID: %v", err)
			return 0, 0, consts.ErrDBUpdateFailed
		}

		// If we have a preserved UIDVALIDITY, update it
		if options.PreservedUIDValidity != nil {
			_, err = tx.Exec(ctx, `
				UPDATE mailboxes 
				SET uid_validity = $2
				WHERE id = $1`,
				options.MailboxID, *options.PreservedUIDValidity)
			if err != nil {
				log.Printf("[DB] failed to update UIDVALIDITY: %v", err)
				return 0, 0, consts.ErrDBUpdateFailed
			}
		}
	} else {
		// Atomically increment and get the new highest UID for the mailbox.
		err = tx.QueryRow(ctx, `UPDATE mailboxes SET highest_uid = highest_uid + 1 WHERE id = $1 RETURNING highest_uid`, options.MailboxID).Scan(&highestUID)
		if err != nil {
			log.Printf("[DB] failed to update highest UID: %v", err)
			return 0, 0, consts.ErrDBUpdateFailed
		}
		uidToUse = highestUID
	}

	recipientsJSON, err := json.Marshal(options.Recipients)
	if err != nil {
		log.Printf("[DB] failed to marshal recipients: %v", err)
		return 0, 0, consts.ErrSerializationFailed
	}

	// Prepare denormalized sort fields for faster sorting.
	var subjectSort, fromNameSort, fromEmailSort, toEmailSort, ccEmailSort string
	subjectSort = strings.ToUpper(helpers.SanitizeUTF8(options.Subject))

	var fromFound, toFound, ccFound bool
	for _, r := range options.Recipients {
		switch r.AddressType {
		case "from":
			if !fromFound {
				fromNameSort = strings.ToLower(r.Name)
				fromEmailSort = strings.ToLower(r.EmailAddress)
				fromFound = true
			}
		case "to":
			if !toFound {
				toEmailSort = strings.ToLower(r.EmailAddress)
				toFound = true
			}
		case "cc":
			if !ccFound {
				ccEmailSort = strings.ToLower(r.EmailAddress)
				ccFound = true
			}
		}
		if fromFound && toFound && ccFound {
			break
		}
	}

	inReplyToStr := strings.Join(options.InReplyTo, " ")

	systemFlagsToSet, customKeywordsToSet := SplitFlags(options.Flags)
	bitwiseFlags := FlagsToBitwise(systemFlagsToSet)

	var customKeywordsJSON []byte
	if len(customKeywordsToSet) == 0 {
		customKeywordsJSON = []byte("[]")
	} else {
		customKeywordsJSON, err = json.Marshal(customKeywordsToSet)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to marshal custom keywords for InsertMessage: %w", err)
		}
	}

	var messageRowId int64

	// Sanitize inputs
	saneSubject := helpers.SanitizeUTF8(options.Subject)
	saneInReplyToStr := helpers.SanitizeUTF8(inReplyToStr)
	sanePlaintextBody := helpers.SanitizeUTF8(options.PlaintextBody)

	err = tx.QueryRow(ctx, `
		INSERT INTO messages
			(account_id, mailbox_id, mailbox_path, uid, message_id, content_hash, s3_domain, s3_localpart, flags, custom_flags, internal_date, size, subject, sent_date, in_reply_to, body_structure, recipients_json, created_modseq, subject_sort, from_name_sort, from_email_sort, to_email_sort, cc_email_sort)
		VALUES
			(@account_id, @mailbox_id, @mailbox_path, @uid, @message_id, @content_hash, @s3_domain, @s3_localpart, @flags, @custom_flags, @internal_date, @size, @subject, @sent_date, @in_reply_to, @body_structure, @recipients_json, nextval('messages_modseq'), @subject_sort, @from_name_sort, @from_email_sort, @to_email_sort, @cc_email_sort)
		RETURNING id
	`, pgx.NamedArgs{
		"account_id":      options.UserID,
		"mailbox_id":      options.MailboxID,
		"mailbox_path":    options.MailboxName,
		"s3_domain":       options.S3Domain,
		"s3_localpart":    options.S3Localpart,
		"uid":             uidToUse,
		"message_id":      saneMessageID,
		"content_hash":    options.ContentHash,
		"flags":           bitwiseFlags,
		"custom_flags":    customKeywordsJSON,
		"internal_date":   options.InternalDate,
		"size":            options.Size,
		"subject":         saneSubject,
		"sent_date":       options.SentDate,
		"in_reply_to":     saneInReplyToStr,
		"body_structure":  bodyStructureData,
		"recipients_json": recipientsJSON,
		"subject_sort":    subjectSort,
		"from_name_sort":  fromNameSort,
		"from_email_sort": fromEmailSort,
		"to_email_sort":   toEmailSort,
		"cc_email_sort":   ccEmailSort,
	}).Scan(&messageRowId)

	if err != nil {
		// Check for a unique constraint violation specifically on the message_id.
		// Other unique violations (e.g., from triggers) are not recoverable here
		// as they will abort the transaction.
		if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "23505" && pgErr.ConstraintName == "messages_message_id_mailbox_id_key" {
			// Unique constraint violation. Check if it's due to message_id and if we can return the existing message.
			// The saneMessageID was used in the INSERT attempt.

			log.Printf("[DB] unique constraint violation for MessageID '%s' in MailboxID %d. Attempting to find existing message.", saneMessageID, options.MailboxID)
			var existingID, existingUID int64
			// Query within the same transaction. If we return successfully from here,
			// the defer tx.Rollback(ctx) will roll back the attempted INSERT and UID bump.
			queryErr := tx.QueryRow(ctx,
				`SELECT id, uid FROM messages 
					 WHERE account_id = $1 AND mailbox_id = $2 AND message_id = $3 AND expunged_at IS NULL`,
				options.UserID, options.MailboxID, saneMessageID).Scan(&existingID, &existingUID)

			if queryErr == nil {
				log.Printf("[DB] found existing message for MessageID '%s' in MailboxID %d. Returning existing ID: %d, UID: %d. Current transaction will be rolled back.", saneMessageID, options.MailboxID, existingID, existingUID)
				return existingID, existingUID, nil // Return existing message details
			} else if errors.Is(queryErr, pgx.ErrNoRows) {
				// This is unexpected: unique constraint fired, but we can't find the row by message_id.
				// Could be a conflict on UID or another unique constraint.
				log.Printf("[DB] unique constraint violation for MailboxID %d (MessageID '%s'), but no existing non-expunged message found by this MessageID. Falling back to unique violation error. Lookup error: %v", options.MailboxID, saneMessageID, queryErr)
			} else {
				// Error during the lookup query
				log.Printf("[DB] error querying for existing message after unique constraint violation (MailboxID %d, MessageID '%s'): %v. Falling back to unique violation error.", options.MailboxID, saneMessageID, queryErr)
			}

			// Fallback to returning the original unique violation error if MessageID was empty or lookup failed.
			log.Printf("[DB] original unique constraint violation error for MailboxID %d, MessageID '%s': %v", options.MailboxID, saneMessageID, err)
			return 0, 0, consts.ErrDBUniqueViolation // Original error
		}
		log.Printf("[DB] failed to insert message into database: %v", err)
		return 0, 0, consts.ErrDBInsertFailed
	}

	// Insert into message_contents. ON CONFLICT DO NOTHING handles content deduplication.
	// We always store headers for FTS. For the body, if the message is older than the FTS
	// retention period, we store NULL to save space but still generate the TSV for searching.
	var textBodyArg any = sanePlaintextBody
	if options.FTSRetention > 0 && options.SentDate.Before(time.Now().Add(-options.FTSRetention)) {
		textBodyArg = nil
	}

	// For old messages, textBodyArg is NULL, but sanePlaintextBody is used for TSV generation.
	_, err = tx.Exec(ctx, `
		INSERT INTO message_contents (content_hash, text_body, text_body_tsv, headers, headers_tsv)
		VALUES ($1, $2, to_tsvector('simple', $3), $4, to_tsvector('simple', $4))
		ON CONFLICT (content_hash) DO NOTHING
	`, options.ContentHash, textBodyArg, sanePlaintextBody, options.RawHeaders)
	if err != nil {
		log.Printf("[DB] failed to insert message content for content_hash %s: %v", options.ContentHash, err)
		return 0, 0, consts.ErrDBInsertFailed // Transaction will rollback
	}

	_, err = tx.Exec(ctx, `
	INSERT INTO pending_uploads (instance_id, content_hash, size, created_at, account_id)
	VALUES ($1, $2, $3, $4, $5) ON CONFLICT (content_hash, account_id) DO NOTHING`,
		upload.InstanceID,
		upload.ContentHash,
		upload.Size,
		time.Now(),
		upload.AccountID,
	)
	if err != nil {
		log.Printf("[DB] failed to insert into pending_uploads for content_hash %s: %v", upload.ContentHash, err)
		return 0, 0, consts.ErrDBInsertFailed // Transaction will rollback
	}

	return messageRowId, highestUID, nil
}

func (d *Database) InsertMessageFromImporter(ctx context.Context, tx pgx.Tx, options *InsertMessageOptions) (messageID int64, uid int64, err error) {
	saneMessageID := helpers.SanitizeUTF8(options.MessageID)
	if saneMessageID == "" {
		log.Printf("[DB] messageID is empty after sanitization, generating a new one without modifying the message.")
		// Generate a new message ID if not provided
		saneMessageID = fmt.Sprintf("<%d@%s>", time.Now().UnixNano(), options.MailboxName)
	}

	bodyStructureData, err := helpers.SerializeBodyStructureGob(options.BodyStructure)
	if err != nil {
		log.Printf("[DB] failed to serialize BodyStructure: %v", err)
		return 0, 0, consts.ErrSerializationFailed
	}

	if options.InternalDate.IsZero() {
		options.InternalDate = time.Now()
	}

	var highestUID int64
	var uidToUse int64

	// If we have a preserved UID, use it; otherwise generate a new one
	if options.PreservedUID != nil {
		uidToUse = int64(*options.PreservedUID)

		// Update highest_uid if preserved UID is higher
		err = tx.QueryRow(ctx, `
			UPDATE mailboxes 
			SET highest_uid = GREATEST(highest_uid, $2)
			WHERE id = $1 
			RETURNING highest_uid`,
			options.MailboxID, uidToUse).Scan(&highestUID)
		if err != nil {
			log.Printf("[DB] failed to update highest UID with preserved UID: %v", err)
			return 0, 0, consts.ErrDBUpdateFailed
		}

		// If we have a preserved UIDVALIDITY, update it
		if options.PreservedUIDValidity != nil {
			_, err = tx.Exec(ctx, `
				UPDATE mailboxes 
				SET uid_validity = $2
				WHERE id = $1`,
				options.MailboxID, *options.PreservedUIDValidity)
			if err != nil {
				log.Printf("[DB] failed to update UIDVALIDITY: %v", err)
				return 0, 0, consts.ErrDBUpdateFailed
			}
		}
	} else {
		// Atomically increment and get the new highest UID for the mailbox.
		// The UPDATE statement implicitly locks the row, making a prior SELECT FOR UPDATE redundant.
		err = tx.QueryRow(ctx, `UPDATE mailboxes SET highest_uid = highest_uid + 1 WHERE id = $1 RETURNING highest_uid`, options.MailboxID).Scan(&highestUID)
		if err != nil {
			log.Printf("[DB] failed to update highest UID: %v", err)
			return 0, 0, consts.ErrDBUpdateFailed
		}
		uidToUse = highestUID
	}

	recipientsJSON, err := json.Marshal(options.Recipients)
	if err != nil {
		log.Printf("[DB] failed to marshal recipients: %v", err)
		return 0, 0, consts.ErrSerializationFailed
	}

	// Prepare denormalized sort fields for faster sorting.
	var subjectSort, fromNameSort, fromEmailSort, toEmailSort, ccEmailSort string
	subjectSort = strings.ToUpper(helpers.SanitizeUTF8(options.Subject))

	var fromFound, toFound, ccFound bool
	for _, r := range options.Recipients {
		switch r.AddressType {
		case "from":
			if !fromFound {
				fromNameSort = strings.ToLower(r.Name)
				fromEmailSort = strings.ToLower(r.EmailAddress)
				fromFound = true
			}
		case "to":
			if !toFound {
				toEmailSort = strings.ToLower(r.EmailAddress)
				toFound = true
			}
		case "cc":
			if !ccFound {
				ccEmailSort = strings.ToLower(r.EmailAddress)
				ccFound = true
			}
		}
		if fromFound && toFound && ccFound {
			break
		}
	}

	inReplyToStr := strings.Join(options.InReplyTo, " ")

	systemFlagsToSet, customKeywordsToSet := SplitFlags(options.Flags)
	bitwiseFlags := FlagsToBitwise(systemFlagsToSet)

	var customKeywordsJSON []byte
	if len(customKeywordsToSet) == 0 {
		customKeywordsJSON = []byte("[]")
	} else {
		customKeywordsJSON, err = json.Marshal(customKeywordsToSet)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to marshal custom keywords for InsertMessage: %w", err)
		}
	}

	var messageRowId int64

	// Sanitize inputs
	saneSubject := helpers.SanitizeUTF8(options.Subject)
	saneInReplyToStr := helpers.SanitizeUTF8(inReplyToStr)
	sanePlaintextBody := helpers.SanitizeUTF8(options.PlaintextBody)

	err = tx.QueryRow(ctx, `
		INSERT INTO messages
			(account_id, mailbox_id, mailbox_path, uid, message_id, content_hash, s3_domain, s3_localpart, flags, custom_flags, internal_date, size, subject, sent_date, in_reply_to, body_structure, recipients_json, uploaded, created_modseq, subject_sort, from_name_sort, from_email_sort, to_email_sort, cc_email_sort)
		VALUES
			(@account_id, @mailbox_id, @mailbox_path, @uid, @message_id, @content_hash, @s3_domain, @s3_localpart, @flags, @custom_flags, @internal_date, @size, @subject, @sent_date, @in_reply_to, @body_structure, @recipients_json, true, nextval('messages_modseq'), @subject_sort, @from_name_sort, @from_email_sort, @to_email_sort, @cc_email_sort)
		RETURNING id
	`, pgx.NamedArgs{
		"account_id":      options.UserID,
		"mailbox_id":      options.MailboxID,
		"mailbox_path":    options.MailboxName,
		"s3_domain":       options.S3Domain,
		"s3_localpart":    options.S3Localpart,
		"uid":             uidToUse,
		"message_id":      saneMessageID,
		"content_hash":    options.ContentHash,
		"flags":           bitwiseFlags,
		"custom_flags":    customKeywordsJSON,
		"internal_date":   options.InternalDate,
		"size":            options.Size,
		"subject":         saneSubject,
		"sent_date":       options.SentDate,
		"in_reply_to":     saneInReplyToStr,
		"body_structure":  bodyStructureData,
		"recipients_json": recipientsJSON,
		"subject_sort":    subjectSort,
		"from_name_sort":  fromNameSort,
		"from_email_sort": fromEmailSort,
		"to_email_sort":   toEmailSort,
		"cc_email_sort":   ccEmailSort,
	}).Scan(&messageRowId)

	if err != nil {
		// Check for a unique constraint violation specifically on the message_id.
		// Other unique violations (e.g., from triggers) are not recoverable here
		// as they will abort the transaction.
		if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == "23505" && pgErr.ConstraintName == "messages_message_id_mailbox_id_key" {
			// Unique constraint violation. Check if it's due to message_id and if we can return the existing message.
			// The saneMessageID was used in the INSERT attempt.

			log.Printf("[DB] unique constraint violation for MessageID '%s' in MailboxID %d. Attempting to find existing message.", saneMessageID, options.MailboxID)
			var existingID, existingUID int64
			// Query within the same transaction. If we return successfully from here,
			// the defer tx.Rollback(ctx) will roll back the attempted INSERT and UID bump.
			queryErr := tx.QueryRow(ctx,
				`SELECT id, uid FROM messages 
					 WHERE account_id = $1 AND mailbox_id = $2 AND message_id = $3 AND expunged_at IS NULL`,
				options.UserID, options.MailboxID, saneMessageID).Scan(&existingID, &existingUID)

			if queryErr == nil {
				log.Printf("[DB] found existing message for MessageID '%s' in MailboxID %d. Returning existing ID: %d, UID: %d. Current transaction will be rolled back.", saneMessageID, options.MailboxID, existingID, existingUID)
				return existingID, existingUID, nil // Return existing message details
			} else if errors.Is(queryErr, pgx.ErrNoRows) {
				// This is unexpected: unique constraint fired, but we can't find the row by message_id.
				// Could be a conflict on UID or another unique constraint.
				log.Printf("[DB] unique constraint violation for MailboxID %d (MessageID '%s'), but no existing non-expunged message found by this MessageID. Falling back to unique violation error. Lookup error: %v", options.MailboxID, saneMessageID, queryErr)
			} else {
				// Error during the lookup query
				log.Printf("[DB] error querying for existing message after unique constraint violation (MailboxID %d, MessageID '%s'): %v. Falling back to unique violation error.", options.MailboxID, saneMessageID, queryErr)
			}

			// Fallback to returning the original unique violation error if MessageID was empty or lookup failed.
			log.Printf("[DB] original unique constraint violation error for MailboxID %d, MessageID '%s': %v", options.MailboxID, saneMessageID, err)
			return 0, 0, consts.ErrDBUniqueViolation // Original error
		}
		log.Printf("[DB] failed to insert message into database: %v", err)
		return 0, 0, consts.ErrDBInsertFailed
	}

	// Insert into message_contents. ON CONFLICT DO NOTHING handles content deduplication.
	// We always store headers for FTS. For the body, if the message is older than the FTS
	// retention period, we store NULL to save space but still generate the TSV for searching.
	var textBodyArg interface{} = sanePlaintextBody
	if options.FTSRetention > 0 && options.SentDate.Before(time.Now().Add(-options.FTSRetention)) {
		textBodyArg = nil
	}

	// For old messages, textBodyArg is NULL, but sanePlaintextBody is used for TSV generation.
	_, err = tx.Exec(ctx, `
		INSERT INTO message_contents (content_hash, text_body, text_body_tsv, headers, headers_tsv)
		VALUES ($1, $2, to_tsvector('simple', $3), $4, to_tsvector('simple', $4))
		ON CONFLICT (content_hash) DO NOTHING
	`, options.ContentHash, textBodyArg, sanePlaintextBody, options.RawHeaders)
	if err != nil {
		log.Printf("[DB] failed to insert message content for content_hash %s: %v", options.ContentHash, err)
		return 0, 0, consts.ErrDBInsertFailed // Transaction will rollback
	}

	return messageRowId, uidToUse, nil
}
