package persistence

import (
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"github.com/CovidShield/server/pkg/config"
	"github.com/CovidShield/server/pkg/timemath"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Shopify/goose/logger"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/nacl/box"
)

func TestDeleteOldDiagnosisKeys(t *testing.T) {
	// Init config
	config.InitConfig()

	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	oldestDateNumber := timemath.DateNumber(time.Now()) - config.AppConstants.MaxDiagnosisKeyRetentionDays
	oldestHour := timemath.HourNumberAtStartOfDate(oldestDateNumber)

	mock.ExpectExec(`DELETE FROM diagnosis_keys WHERE hour_of_submission < ?`).WithArgs(oldestHour).WillReturnResult(sqlmock.NewResult(1, 1))
	deleteOldDiagnosisKeys(db)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

}

func TestDeleteOldEncryptionKeys(t *testing.T) {

	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	query := fmt.Sprintf(`
		DELETE FROM encryption_keys
		WHERE  (created < (NOW() - INTERVAL %d DAY))
		OR    ((created < (NOW() - INTERVAL %d MINUTE)) AND app_public_key IS NULL)
		OR    remaining_keys = 0
	`, config.AppConstants.EncryptionKeyValidityDays, config.AppConstants.OneTimeCodeExpiryInMinutes)

	mock.ExpectExec(query).WillReturnResult(sqlmock.NewResult(1, 1))
	deleteOldEncryptionKeys(db)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

}

func TestCaimKey(t *testing.T) {

	pub, _, _ := box.GenerateKey(rand.Reader)
	oneTimeCode := "80311300"

	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	// If query fails rollback transaction
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnError(fmt.Errorf("error"))
	mock.ExpectRollback()
	_, receivedErr := claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr := fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if could not query for key")

	// If app key exists
	mock.ExpectBegin()
	rows := sqlmock.NewRows([]string{"count"}).AddRow(1)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)
	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = ErrDuplicateKey
	assert.Equal(t, expectedErr, receivedErr, "Expected ErrDuplicateKey if there are duplicate keys")

	// App key does not exist, but created is not correct
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	rows = sqlmock.NewRows([]string{"created"}).AddRow("1950-01-01 00:00:00")
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = ErrInvalidOneTimeCode
	assert.Equal(t, expectedErr, receivedErr, "Expected ErrInvalidOneTimeCode if time code is not valid")

	// Prepare update fails
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	rows = sqlmock.NewRows([]string{"created"}).AddRow(time.Now())
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	query := fmt.Sprintf(
		`UPDATE encryption_keys
		SET one_time_code = NULL,
			app_public_key = ?,
			created = ?
		WHERE one_time_code = ?
		AND created > (NOW() - INTERVAL %d MINUTE)`,
		config.AppConstants.OneTimeCodeExpiryInMinutes,
	)

	mock.ExpectPrepare(query).WillReturnError(fmt.Errorf("error"))

	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if could not prepare update")

	// Execute fails after update
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	created := time.Now()

	rows = sqlmock.NewRows([]string{"created"}).AddRow(created)
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	created = timemath.MostRecentUTCMidnight(created)

	mock.ExpectPrepare(query).ExpectExec().WithArgs(pub[:], created, oneTimeCode).WillReturnError(fmt.Errorf("error"))

	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if could not execute update")

	// RowsAffected is not equal to 1
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	created = time.Now()

	rows = sqlmock.NewRows([]string{"created"}).AddRow(created)
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	created = timemath.MostRecentUTCMidnight(created)

	mock.ExpectPrepare(query).ExpectExec().WithArgs(pub[:], created, oneTimeCode).WillReturnResult(sqlmock.NewResult(1, 2))

	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = ErrInvalidOneTimeCode
	assert.Equal(t, expectedErr, receivedErr, "Expected ErrInvalidOneTimeCode if rowsAffected was not 1")

	// Getting public key throws an error
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	created = time.Now()

	rows = sqlmock.NewRows([]string{"created"}).AddRow(created)
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	created = timemath.MostRecentUTCMidnight(created)

	mock.ExpectPrepare(query).ExpectExec().WithArgs(pub[:], created, oneTimeCode).WillReturnResult(sqlmock.NewResult(1, 1))

	mock.ExpectPrepare(`SELECT server_public_key FROM encryption_keys WHERE app_public_key = ?`).ExpectQuery().WithArgs(pub[:]).WillReturnError(fmt.Errorf("error"))

	mock.ExpectRollback()
	_, receivedErr = claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if server_public_key was not queried")

	// Commits and returns a server key
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"count"}).AddRow(0)
	mock.ExpectQuery(`SELECT COUNT(*) FROM encryption_keys WHERE app_public_key = ?`).WithArgs(pub[:]).WillReturnRows(rows)

	created = time.Now()

	rows = sqlmock.NewRows([]string{"created"}).AddRow(created)
	mock.ExpectQuery(`SELECT created FROM encryption_keys WHERE one_time_code = ?`).WithArgs(oneTimeCode).WillReturnRows(rows)

	created = timemath.MostRecentUTCMidnight(created)

	mock.ExpectPrepare(query).ExpectExec().WithArgs(pub[:], created, oneTimeCode).WillReturnResult(sqlmock.NewResult(1, 1))

	rows = sqlmock.NewRows([]string{"server_public_key"}).AddRow(pub[:])
	mock.ExpectPrepare(`SELECT server_public_key FROM encryption_keys WHERE app_public_key = ?`).ExpectQuery().WithArgs(pub[:]).WillReturnRows(rows)

	mock.ExpectCommit()

	serverKey, _ := claimKey(db, oneTimeCode, pub[:])

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	assert.Equal(t, pub[:], serverKey, "should return server key")

}

func TestPersistEncryptionKey(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	// Capture logs
	oldLog := log
	defer func() { log = oldLog }()

	nullLog, hook := test.NewNullLogger()
	nullLog.ExitFunc = func(code int) {}

	log = func(ctx logger.Valuer, err ...error) *logrus.Entry {
		return logrus.NewEntry(nullLog)
	}

	region := "302"
	originator := "randomOrigin"
	hashID := ""
	pub, priv, _ := box.GenerateKey(rand.Reader)
	oneTimeCode := "80311300"

	// Rolls back if insert without HashID fails
	mock.ExpectBegin()
	mock.ExpectExec(
		`INSERT INTO encryption_keys
		(region, originator, hash_id, server_private_key, server_public_key, one_time_code, remaining_keys)
		VALUES (?, ?, ?, ?, ?, ?, ?)`).WithArgs(
		region,
		originator,
		hashID,
		priv[:],
		pub[:],
		oneTimeCode,
		config.AppConstants.InitialRemainingKeys,
	).WillReturnError(fmt.Errorf("error"))
	mock.ExpectRollback()

	receivedErr := persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr := fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if could not execute update")

	// Commits if insert without HashID
	mock.ExpectBegin()
	mock.ExpectExec(
		`INSERT INTO encryption_keys
		(region, originator, hash_id, server_private_key, server_public_key, one_time_code, remaining_keys)
		VALUES (?, ?, ?, ?, ?, ?, ?)`).WithArgs(
		region,
		originator,
		hashID,
		priv[:],
		pub[:],
		oneTimeCode,
		config.AppConstants.InitialRemainingKeys,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	receivedResult := persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	assert.Nil(t, receivedResult, "Expected error if could not execute insert")

	hashID = "abcd"

	// Commit if HashID is unique
	mock.ExpectBegin()
	mock.ExpectQuery(
		`SELECT one_time_code FROM encryption_keys WHERE hash_id = ? FOR UPDATE`).WithArgs(hashID).WillReturnRows(sqlmock.NewRows([]string{"one_time_code"}))

	mock.ExpectExec(
		`INSERT INTO encryption_keys
			(region, originator, hash_id, server_private_key, server_public_key, one_time_code, remaining_keys)
			VALUES (?, ?, ?, ?, ?, ?, ?)`).WithArgs(
		region,
		originator,
		hashID,
		priv[:],
		pub[:],
		oneTimeCode,
		config.AppConstants.InitialRemainingKeys,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	receivedResult = persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	assert.Nil(t, receivedResult, "Expected nil if new HashID is passed")

	// Rolls back if insert fails because the table is locked
	mock.ExpectBegin()
	mock.ExpectQuery(
		`SELECT one_time_code FROM encryption_keys WHERE hash_id = ? FOR UPDATE`).WithArgs(hashID).WillReturnError(fmt.Errorf("table locked"))
	mock.ExpectRollback()

	receivedErr = persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("table locked")
	assert.Equal(t, expectedErr, receivedErr, "Expected table locked error if the select fails")

	assert.Equal(t, 1, len(hook.Entries))
	assert.Equal(t, logrus.ErrorLevel, hook.LastEntry().Level)
	assert.Equal(t, "table locked", hook.LastEntry().Message)
	hook.Reset()

	// Rolls back if a used HashID is found
	mock.ExpectBegin()

	rows := sqlmock.NewRows([]string{"one_time_code"}).AddRow(nil)
	mock.ExpectQuery(
		`SELECT one_time_code FROM encryption_keys WHERE hash_id = ? FOR UPDATE`).WithArgs(hashID).WillReturnRows(rows)
	mock.ExpectRollback()

	receivedErr = persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("used hashID found")
	assert.Equal(t, expectedErr, receivedErr, "Expected used hashID found error if the select fails")

	// Rolls back if a un-used HashID is found and delete fails
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"one_time_code"}).AddRow(oneTimeCode)
	mock.ExpectQuery(
		`SELECT one_time_code FROM encryption_keys WHERE hash_id = ? FOR UPDATE`).WithArgs(hashID).WillReturnRows(rows)
	mock.ExpectExec(`DELETE FROM encryption_keys WHERE hash_id = ? AND one_time_code IS NOT NULL`).WithArgs(hashID).WillReturnError(fmt.Errorf("error"))
	mock.ExpectRollback()

	receivedErr = persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	expectedErr = fmt.Errorf("error")
	assert.Equal(t, expectedErr, receivedErr, "Expected error if could not delete un-used HashID")

	// Commits if a un-used HashID is found and delete passes
	mock.ExpectBegin()
	rows = sqlmock.NewRows([]string{"one_time_code"}).AddRow(oneTimeCode)
	mock.ExpectQuery(
		`SELECT one_time_code FROM encryption_keys WHERE hash_id = ? FOR UPDATE`).WithArgs(hashID).WillReturnRows(rows)
	mock.ExpectExec(`DELETE FROM encryption_keys WHERE hash_id = ? AND one_time_code IS NOT NULL`).WithArgs(hashID).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(
		`INSERT INTO encryption_keys
			(region, originator, hash_id, server_private_key, server_public_key, one_time_code, remaining_keys)
			VALUES (?, ?, ?, ?, ?, ?, ?)`).WithArgs(
		region,
		originator,
		hashID,
		priv[:],
		pub[:],
		oneTimeCode,
		config.AppConstants.InitialRemainingKeys,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	receivedResult = persistEncryptionKey(db, region, originator, hashID, pub, priv, oneTimeCode)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}

	assert.Nil(t, receivedResult, "Expected nil if new OTC could be generated with un-used HashID")
}

func TestPrivForPub(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	pub, priv, _ := box.GenerateKey(rand.Reader)

	query := fmt.Sprintf(`
	SELECT server_private_key FROM encryption_keys
		WHERE server_public_key = ?
		AND created > (NOW() - INTERVAL %d DAY)
		LIMIT 1`,
		config.AppConstants.EncryptionKeyValidityDays,
	)

	rows := sqlmock.NewRows([]string{"server_private_key"}).AddRow(priv[:])
	mock.ExpectQuery(query).WithArgs(pub[:]).WillReturnRows(rows)

	expectedResult := priv[:]
	var receivedResult []byte
	privForPub(db, pub[:]).Scan(&receivedResult)

	assert.Equal(t, expectedResult, receivedResult, "Expected private key for public key")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}

func TestDiagnosisKeysForHours(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	defer db.Close()

	region := "302"
	startHour := uint32(100)
	endHour := uint32(200)
	currentRollingStartIntervalNumber := int32(2651450)
	minRollingStartIntervalNumber := timemath.RollingStartIntervalNumberPlusDays(currentRollingStartIntervalNumber, -14)

	query := `
	SELECT region, key_data, rolling_start_interval_number, rolling_period, transmission_risk_level FROM diagnosis_keys
		WHERE hour_of_submission >= ?
		AND hour_of_submission < ?
		AND rolling_start_interval_number > ?
		AND region = ?
		ORDER BY key_data`

	row := sqlmock.NewRows([]string{"region", "key_data", "rolling_start_interval_number", "rolling_period", "transmission_risk_level"}).AddRow("302", []byte{}, 2651450, 144, 4)
	mock.ExpectQuery(query).WithArgs(
		startHour,
		endHour,
		minRollingStartIntervalNumber,
		region).WillReturnRows(row)

	expectedResult := []byte("302")
	rows, _ := diagnosisKeysForHours(db, region, startHour, endHour, currentRollingStartIntervalNumber)
	var receivedResult []byte
	for rows.Next() {
		rows.Scan(&receivedResult, nil, nil, nil, nil)
	}

	assert.Equal(t, expectedResult, receivedResult, "Expected rows for the query")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("there were unfulfilled expectations: %s", err)
	}
}