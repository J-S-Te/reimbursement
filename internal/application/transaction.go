package application

import (
	"context"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
	"time"
)

// localTransaction 对MySQL死锁与锁等待超时执行至多三次本地事务，失败尝试全部回滚。
// 参数：ctx 为即时取消边界，db 为请求数据库，work 为不含网络调用的同命令本地事务。
// 返回值：提交结果、最终错误或上下文取消；不冻结失败或变更命令标识。
func localTransaction(ctx context.Context, db *gorm.DB, work func(*gorm.DB) error) error {
	for attempt := 0; attempt < 3; attempt++ {
		if e := ctx.Err(); e != nil {
			return e
		}
		err := db.WithContext(ctx).Transaction(work)
		if err == nil {
			return nil
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		var failure *driver.MySQLError
		if !errors.As(err, &failure) || (failure.Number != 1213 && failure.Number != 1205) || attempt == 2 {
			return err
		}
		timer := time.NewTimer(time.Duration(10*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
