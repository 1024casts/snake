package user

import (
	"context"
	"fmt"
	"time"

	"github.com/1024casts/snake/pkg/cache"

	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
	"golang.org/x/sync/singleflight"

	"github.com/1024casts/snake/internal/cache/user"
	"github.com/1024casts/snake/internal/model"
	"github.com/1024casts/snake/pkg/log"
)

var (
	ErrNotFound = gorm.ErrRecordNotFound
)

// BaseRepo 定义用户仓库接口
type BaseRepo interface {
	CreateUser(ctx context.Context, user model.UserBaseModel) (id uint64, err error)
	UpdateUser(ctx context.Context, id uint64, userMap map[string]interface{}) error
	GetOneUser(ctx context.Context, id uint64) (*model.UserBaseModel, error)
	GetUsersByIds(ctx context.Context, ids []uint64) ([]*model.UserBaseModel, error)
	GetUserByPhone(ctx context.Context, phone int64) (*model.UserBaseModel, error)
	GetUserByEmail(ctx context.Context, email string) (*model.UserBaseModel, error)
	Close()
}

// userBaseRepo 用户仓库
type userBaseRepo struct {
	db        *gorm.DB
	userCache *user.Cache
}

// NewUserRepo 实例化用户仓库
func NewUserRepo(db *gorm.DB) BaseRepo {
	return &userBaseRepo{
		db:        db,
		userCache: user.NewUserCache(),
	}
}

// Create 创建用户
func (repo *userBaseRepo) CreateUser(ctx context.Context, user model.UserBaseModel) (id uint64, err error) {
	err = repo.db.Create(&user).Error
	if err != nil {
		return 0, errors.Wrap(err, "[repo.user_base] create user err")
	}

	return user.ID, nil
}

// Update 更新用户信息
func (repo *userBaseRepo) UpdateUser(ctx context.Context, id uint64, userMap map[string]interface{}) error {
	user, err := repo.GetOneUser(ctx, id)
	if err != nil {
		return errors.Wrap(err, "[repo.user_base] update user data err")
	}

	// 删除cache
	err = repo.userCache.DelUserBaseCache(id)
	if err != nil {
		log.Warnf("[repo.user_base] delete user cache err: %v", err)
	}

	return repo.db.Model(user).Updates(userMap).Error
}

// GetUserByID 获取用户
// 缓存的更新策略使用 Cache Aside Pattern
// see: https://coolshell.cn/articles/17416.html
func (repo *userBaseRepo) GetOneUser(ctx context.Context, uid uint64) (userBase *model.UserBaseModel, err error) {
	//var userBase *model.UserBaseModel
	start := time.Now()
	defer func() {
		log.Infof("[repo.user_base] get user by uid: %d cost: %d μs", uid, time.Since(start).Microseconds())
	}()
	// 从cache获取
	userBase, err = repo.userCache.GetUserBaseCache(uid)
	if err != nil {
		if err == cache.ErrPlaceholder {
			return nil, ErrNotFound
		} else if err != ErrNotFound {
			// fail fast
			return nil, err
		}
		// if cache error return, don't request to db
		log.Warnf("[repo.user_base] get user by uid err: %v, uid: %d", err, uid)
		return
	}
	// hit cache
	if userBase != nil {
		// add cache hit

		log.Infof("[repo.user_base] get user base data from cache, uid: %d", uid)
		return
	}

	// use sync/singleflight mode to get data
	// demo see: https://github.com/go-demo/singleflight-demo/blob/master/main.go
	// https://juejin.cn/post/6844904084445593613
	getDataFn := func() (interface{}, error) {
		data := new(model.UserBaseModel)
		// 从数据库中获取
		err = repo.db.First(data, uid).Error
		// if data is empty, set not found cache to prevent cache penetration(缓存穿透)
		if errors.Is(err, ErrNotFound) {
			err = repo.userCache.SetCacheWithNotFound(uid)
			if err != nil {
				log.Warnf("[repo.user_base] SetCacheWithNotFound err, uid: %d", uid)
			}
			return nil, ErrNotFound
		} else if err != nil {
			// add metric for db err

			return nil, err
		}

		// set cache
		err = repo.userCache.SetUserBaseCache(uid, data, cache.DefaultExpireTime)
		if err != nil {
			return data, errors.Wrap(err, "[repo.user_base] set user base data err")
		}
		return data, nil
	}

	g := singleflight.Group{}
	doKey := fmt.Sprintf("get_user_base_%d", uid)
	val, err, _ := g.Do(doKey, getDataFn)
	if err != nil {
		return nil, errors.Wrap(err, "[repo.user_base] get user base err via single flight do")
	}
	data := val.(*model.UserBaseModel)

	// add cache miss

	return data, nil
}

// GetUsersByIds 批量获取用户
func (repo *userBaseRepo) GetUsersByIds(ctx context.Context, userIDs []uint64) ([]*model.UserBaseModel, error) {
	users := make([]*model.UserBaseModel, 0)

	// 从cache批量获取
	userCacheMap, err := repo.userCache.MultiGetUserBaseCache(userIDs)
	if err != nil {
		return users, errors.Wrap(err, "[repo.user_base] multi get user cache data err")
	}

	// 查询未命中
	for _, userID := range userIDs {
		idx := repo.userCache.GetUserBaseCacheKey(userID)
		userModel, ok := userCacheMap[idx]
		if !ok {
			userModel, err = repo.GetOneUser(ctx, userID)
			if err != nil {
				log.Warnf("[repo.user_base] get user model err: %v", err)
				continue
			}
		}
		users = append(users, userModel)
	}
	return users, nil
}

// GetUserByPhone 根据手机号获取用户
func (repo *userBaseRepo) GetUserByPhone(ctx context.Context, phone int64) (*model.UserBaseModel, error) {
	user := model.UserBaseModel{}
	err := repo.db.Where("phone = ?", phone).First(&user).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, errors.Wrap(err, "[repo.user_base] get user err by phone")
	}

	return &user, nil
}

// GetUserByEmail 根据邮箱获取手机号
func (repo *userBaseRepo) GetUserByEmail(ctx context.Context, email string) (*model.UserBaseModel, error) {
	userBase := model.UserBaseModel{}
	err := repo.db.Where("email = ?", email).First(&userBase).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return nil, errors.Wrap(err, "[repo.user_base] get user err by email")
	}

	return &userBase, nil
}

// Close close db
func (repo *userBaseRepo) Close() {
	_ = repo.db.Close()
}
